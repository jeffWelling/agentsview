package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

const archiveFormatSessionRefs = 1 << 23
const knownArchiveFormatFlags = archiveFormatSessionRefs

type archiveFormatSpec struct {
	key   string
	bit   int
	modes []string
}

var archiveFormatSpecs = []archiveFormatSpec{
	{"conversation_session_refs", archiveFormatSessionRefs, []string{"v1"}},
}

type archiveFormatQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func archiveFormatFlagsTx(ctx context.Context, tx *sql.Tx) (int, error) {
	return archiveFormatFlags(ctx, tx)
}

func archiveFormatFlags(ctx context.Context, q archiveFormatQuerier) (int, error) {
	return archiveFormatFlagsInSchema(ctx, q, "main")
}

// schema is an internal, fixed attachment name, never user input.
func archiveFormatFlagsInSchema(ctx context.Context, q archiveFormatQuerier, schema string) (int, error) {
	var exists bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+schema+`.sqlite_master WHERE type='table' AND name='archive_metadata')`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	flags := 0
	for _, spec := range archiveFormatSpecs {
		var mode string
		err := q.QueryRowContext(ctx, `SELECT value FROM `+schema+`.archive_metadata WHERE key=?`, spec.key).Scan(&mode)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if !slices.Contains(spec.modes, mode) {
			return 0, fmt.Errorf("unsupported archive storage format %s=%q", spec.key, mode)
		}
		flags |= spec.bit
	}
	return flags, nil
}

func validateArchiveFormatState(ctx context.Context, q archiveFormatQuerier) error {
	return validateArchiveFormatStateInSchema(ctx, q, "main")
}

func validateArchiveFormatStateInSchema(ctx context.Context, q archiveFormatQuerier, schema string) error {
	var version int
	if err := q.QueryRowContext(ctx, "PRAGMA "+schema+".user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 {
		return fmt.Errorf("unsupported negative archive storage version")
	}
	flags, err := archiveFormatFlagsInSchema(ctx, q, schema)
	if err != nil {
		return err
	}
	if version&knownArchiveFormatFlags != flags {
		return fmt.Errorf("archive storage format metadata disagrees with user_version")
	}
	return nil
}

// ATTACH bypasses normal open validation. Validate before reading source rows or
// copying metadata, including when the source predates archive_metadata.
func validateAttachedArchiveFormat(ctx context.Context, q archiveFormatQuerier, schema string) error {
	var version int
	if err := q.QueryRowContext(ctx, "PRAGMA "+schema+".user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 {
		return fmt.Errorf("unsupported negative source archive storage version")
	}
	parserVersion, _ := DecodeUserVersion(version)
	if parserVersion > dataVersion {
		return fmt.Errorf("source archive uses a newer unsupported data or storage format")
	}
	if err := validateArchiveSchemaCompatibility(ctx, q, schema); err != nil {
		return err
	}
	return validateArchiveFormatStateInSchema(ctx, q, schema)
}

// OpenForStorageMaintenance opens an existing current archive without schema
// migrations, identity adoption, backfills, or background work. Its caller must
// hold the archive writer-owner lock for the returned handle's lifetime.
func OpenForStorageMaintenance(ctx context.Context, path string) (*DB, error) {
	d, err := OpenReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*DB, error) { _ = d.Close(); return nil, err }
	version, _, err := readUserVersion(ctx, d.rawReader())
	if err != nil {
		return fail(err)
	}
	if version != dataVersion || d.NeedsResync() {
		return fail(fmt.Errorf("storage maintenance requires a current archive; finish its normal upgrade first"))
	}
	// No first-export initialization: maintenance may also pack a cold index.
	// Require the complete current shape before opening any writer connection.
	rows, err := d.getReader().QueryContext(ctx, `SELECT session_id,message_id,ordinal,role,timestamp,source_id,body,digest,text_bytes,gap,visible_digest,visible_text_bytes,visible_gap,deleted,removed,revision FROM conversation_messages LIMIT 0`)
	if err != nil {
		return fail(err)
	}
	if err := rows.Close(); err != nil {
		return fail(err)
	}
	writer, err := sql.Open(sqliteArchiveDriverName, makeDSN(path, false)+"&mode=rw")
	if err != nil {
		return fail(err)
	}
	writer.SetMaxOpenConns(1)
	if err := configureWALContext(ctx, writer); err != nil {
		_ = writer.Close()
		return fail(err)
	}
	d.readOnly = false
	d.activityKept = newActivityKeptMemo()
	d.sessionStatsFacts = newSessionStatsFactsMemo()
	d.writer.Store(writer)
	return d, nil
}
