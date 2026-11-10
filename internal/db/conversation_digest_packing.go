package db

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const conversationDigestPackingKey = "conversation_digest_packing"
const conversationDigestBatchRows = 256

// Only an opted-in archive may contain BLOB digests. Its format fence reserves
// the entire BLOB storage class for exactly 32 raw digest bytes; legacy TEXT is
// always returned byte for byte, including empty strings and embedded NULs.
func conversationDigestText(value any) (any, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case []byte:
		if len(v) != 32 {
			return nil, fmt.Errorf("invalid packed conversation digest: got %d bytes, require 32", len(v))
		}
		return hex.EncodeToString(v), nil
	default:
		return nil, fmt.Errorf("unsupported conversation digest storage type %T", value)
	}
}

func plainConversationDigest(value any) (any, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	return nil, fmt.Errorf("non-TEXT conversation digest in an archive without digest packing")
}

// Gate BLOB interpretation on the source archive's marker. This also prevents
// an unsupported legacy BLOB32 in a plain archive from silently becoming a
// hexadecimal digest on read or copy. Ordinary TEXT avoids the metadata lookup.
func conversationDigestReadSQL(column, metadata string) string {
	return `CASE WHEN typeof(` + column + `)='text' THEN ` + column +
		` WHEN EXISTS(SELECT 1 FROM ` + metadata + ` WHERE key='conversation_digest_packing' AND value IN ('v1','unpacking')) THEN ` + conversationDigestLogicalSQL(column) + ` ELSE agentsview_plain_digest(` + column + `) END`
}

// Native SQLite handles valid values without crossing the Go callback boundary.
// The error function runs only for unsupported types or malformed BLOBs.
func conversationDigestLogicalSQL(column string) string {
	return `CASE WHEN typeof(` + column + `)='text' THEN ` + column + ` WHEN typeof(` + column + `)='blob' AND length(` + column + `)=32 THEN lower(hex(` + column + `)) ELSE agentsview_digest_text(` + column + `) END`
}

// Copies always cross the archive boundary as logical TEXT. A destination's
// own packing trigger controls its representation, so raw BLOBs never escape
// into an archive without the format fence.
var conversationDigestCopyColumns = `session_id,message_id,ordinal,role,timestamp,source_id,` + conversationDigestReadSQL("digest", "old_db.archive_metadata") + ` AS digest,text_bytes,gap,deleted,removed`

// DigestPackingResult counts rows visited and physically changed in this run.
// File space is reclaimed separately by db compact; this is not a byte-saving
// estimate and interrupted runs may have committed earlier batches.
type DigestPackingResult struct {
	Enabled     bool  `json:"enabled"`
	ScannedRows int64 `json:"scanned_rows"`
	ChangedRows int64 `json:"changed_rows"`
}

// SetConversationDigestPacking explicitly converts a stopped or isolated
// archive in bounded transactions. Callers must hold its writer-owner lock.
// Normal startup never runs this scan. Cancellation leaves committed batches
// queryable, the format fence intact, and a rerun can finish either direction.
func (db *DB) SetConversationDigestPacking(ctx context.Context, enabled bool) (DigestPackingResult, error) {
	result := DigestPackingResult{Enabled: enabled}
	if err := db.lockWriterCtx(ctx); err != nil {
		return result, err
	}
	defer db.mu.Unlock()
	if db.readOnly {
		return result, errors.New("digest packing requires a writable archive")
	}
	if db.writerClosed.Load() {
		return result, ErrWriterClosed
	}
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	mode, err := conversationDigestModeTx(ctx, tx)
	if err != nil {
		return result, err
	}
	if mode == "" {
		// Refuse every pre-existing non-TEXT value before claiming ownership of
		// BLOB32. It may be unrelated legacy data and cannot be reinterpreted.
		var unsupported bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_messages WHERE typeof(digest)!='text' OR typeof(visible_digest)!='text')`).Scan(&unsupported); err != nil {
			return result, err
		}
		if unsupported {
			return result, errors.New("cannot pack conversation digests: existing non-TEXT values require manual investigation")
		}
		if !enabled {
			return result, tx.Commit()
		}
	}
	if err := setConversationDigestModeTx(ctx, tx, enabled); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	var after *int64
	for {
		next, scanned, changed, err := db.convertConversationDigestBatch(ctx, enabled, after)
		if err != nil {
			return result, err
		}
		result.ScannedRows += scanned
		result.ChangedRows += changed
		if scanned == 0 {
			break
		}
		after = &next
	}
	if !enabled {
		tx, err := db.getWriter().BeginTx(ctx, nil)
		if err != nil {
			return result, err
		}
		defer func() { _ = tx.Rollback() }()
		var packed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_messages WHERE typeof(digest)!='text' OR typeof(visible_digest)!='text')`).Scan(&packed); err != nil {
			return result, err
		}
		if packed {
			return result, errors.New("conversation digest reversal incomplete; format fence retained")
		}
		version, _, err := readUserVersion(ctx, tx)
		if err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key=?`, conversationDigestPackingKey); err != nil {
			return result, err
		}
		if err := installConversationDigestTriggers(ctx, tx, ""); err != nil {
			return result, err
		}
		if err := writeUserVersion(ctx, tx, version); err != nil {
			return result, err
		}
		if err := tx.Commit(); err != nil {
			return result, err
		}
	}
	return result, nil
}

func conversationDigestModeTx(ctx context.Context, tx *sql.Tx) (string, error) {
	var mode string
	err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=?`, conversationDigestPackingKey).Scan(&mode)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if mode != "v1" && mode != "unpacking" {
		return "", fmt.Errorf("unsupported conversation digest packing mode %q", mode)
	}
	return mode, nil
}

func setConversationDigestModeTx(ctx context.Context, tx *sql.Tx, enabled bool) error {
	version, _, err := readUserVersion(ctx, tx)
	if err != nil {
		return err
	}
	mode := "unpacking"
	if enabled {
		mode = "v1"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, conversationDigestPackingKey, mode); err != nil {
		return err
	}
	if err := installConversationDigestTriggers(ctx, tx, mode); err != nil {
		return err
	}
	return writeUserVersion(ctx, tx, version)
}

// The caller holds db.mu for the entire maintenance operation. Each batch only
// selects rowids into Go and changes at most 256 rows; no legacy TEXT cell is
// loaded into Go merely to decide whether it is packable.
func (db *DB) convertConversationDigestBatch(ctx context.Context, enabled bool, after *int64) (last, scanned, changed int64, err error) {
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	where := ""
	var args []any
	if after != nil {
		where = " WHERE rowid > ?"
		args = append(args, *after)
	}
	var maxID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT max(rowid),count(*) FROM (SELECT rowid FROM conversation_messages`+where+` ORDER BY rowid LIMIT 256)`, args...).Scan(&maxID, &scanned); err != nil {
		return 0, 0, 0, err
	}
	if scanned == 0 {
		return 0, 0, 0, tx.Commit()
	}
	last = maxID.Int64
	bounds := "rowid <= ?"
	boundArgs := []any{last}
	if after != nil {
		bounds += " AND rowid > ?"
		boundArgs = append(boundArgs, *after)
	}
	var query string
	if enabled {
		query = `UPDATE conversation_messages SET
 digest=CASE WHEN ` + canonicalDigestSQL("digest") + ` THEN unhex(digest) ELSE digest END,
 visible_digest=CASE WHEN ` + canonicalDigestSQL("visible_digest") + ` THEN unhex(visible_digest) ELSE visible_digest END
 WHERE (` + bounds + `) AND (` + canonicalDigestSQL("digest") + ` OR ` + canonicalDigestSQL("visible_digest") + `)`
	} else {
		query = `UPDATE conversation_messages SET digest=` + conversationDigestLogicalSQL("digest") + `,visible_digest=` + conversationDigestLogicalSQL("visible_digest") + ` WHERE (` + bounds + `) AND (typeof(digest)!='text' OR typeof(visible_digest)!='text')`
	}
	result, err := tx.ExecContext(ctx, query, boundArgs...)
	if err != nil {
		return 0, 0, 0, err
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return 0, 0, 0, err
	}
	return last, scanned, changed, tx.Commit()
}

func canonicalDigestSQL(column string) string {
	return `typeof(` + column + `)='text' AND octet_length(` + column + `)=64 AND ` + column + ` NOT GLOB '*[^0-9a-f]*' AND instr(` + column + `,char(0))=0`
}

func installConversationDigestTriggers(ctx context.Context, tx *sql.Tx, mode string) error {
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS conversation_messages_revise;
DROP TRIGGER IF EXISTS conversation_visible_revise;
DROP TRIGGER IF EXISTS conversation_digest_pack_insert;
DROP TRIGGER IF EXISTS conversation_digest_pack_update;
DROP TRIGGER IF EXISTS conversation_digest_validate_insert;
DROP TRIGGER IF EXISTS conversation_digest_validate_update;`); err != nil {
		return err
	}
	stored, visible := conversationSchemaSQL, conversationVisibleRevisionSQL
	if mode != "" {
		// Compare logical values under either SQLite trigger execution order.
		// Physical packing/unpacking and nested trigger updates publish nothing.
		for _, column := range []string{"digest", "visible_digest"} {
			stored = strings.ReplaceAll(stored, "OLD."+column+" IS NOT NEW."+column, conversationDigestLogicalSQL("OLD."+column)+" IS NOT "+conversationDigestLogicalSQL("NEW."+column))
			visible = strings.ReplaceAll(visible, "OLD."+column+" IS NOT NEW."+column, conversationDigestLogicalSQL("OLD."+column)+" IS NOT "+conversationDigestLogicalSQL("NEW."+column))
			visible = strings.ReplaceAll(visible, "OLD."+column+" IS NEW."+column, conversationDigestLogicalSQL("OLD."+column)+" IS "+conversationDigestLogicalSQL("NEW."+column))
		}
	}
	if _, err := tx.ExecContext(ctx, stored+visible); err != nil {
		return err
	}
	if mode == "" {
		return nil
	}
	for _, event := range []string{"INSERT", "UPDATE OF digest,visible_digest"} {
		suffix := "insert"
		if event != "INSERT" {
			suffix = "update"
		}
		if _, err := tx.ExecContext(ctx, `CREATE TRIGGER conversation_digest_validate_`+suffix+` BEFORE `+event+` ON conversation_messages
WHEN typeof(NEW.digest) NOT IN ('text','blob') OR typeof(NEW.visible_digest) NOT IN ('text','blob')
 OR (typeof(NEW.digest)='blob' AND length(NEW.digest)!=32)
 OR (typeof(NEW.visible_digest)='blob' AND length(NEW.visible_digest)!=32)
BEGIN SELECT RAISE(ABORT,'invalid packed conversation digest'); END;`); err != nil {
			return err
		}
		if mode != "v1" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `CREATE TRIGGER conversation_digest_pack_`+suffix+` AFTER `+event+` ON conversation_messages
WHEN (`+canonicalDigestSQL("NEW.digest")+`) OR (`+canonicalDigestSQL("NEW.visible_digest")+`)
BEGIN UPDATE conversation_messages SET
 digest=CASE WHEN `+canonicalDigestSQL("digest")+` THEN unhex(digest) ELSE digest END,
 visible_digest=CASE WHEN `+canonicalDigestSQL("visible_digest")+` THEN unhex(visible_digest) ELSE visible_digest END
 WHERE session_id=NEW.session_id AND message_id=NEW.message_id; END;`); err != nil {
			return err
		}
	}
	return nil
}
