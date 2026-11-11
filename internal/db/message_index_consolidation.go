package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const messageOrdinalIndexSQL = `CREATE UNIQUE INDEX idx_messages_session_ordinal ON messages(session_id, ordinal)`
const messageIndexStageTable = "messages_index_consolidation"

// The hook runs after a real copy, before verification or any source-table
// removal. Tests use it to cancel work or corrupt the disposable candidate.
var messageIndexCopyTestHook func(context.Context, *sql.Tx) error
var messageIndexVacuumTestHook func(string) error

type messageIndexSchemaObject struct {
	typ, name, table, ddl string
}

type messageIndexPlan struct {
	columns      []string
	tableDDL     string
	objects      []messageIndexSchemaObject
	expectedHash string
	changed      bool
}

func checkMessageIndexConsolidationSpace(ctx context.Context, stagingDir string, estimate CompactEstimate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	required := messageIndexStagingBytes(estimate.EstimatedDatabaseBytes)
	same, err := sameFilesystem(stagingDir, estimate.SQLiteTempDir)
	if err != nil {
		return fmt.Errorf("locate SQLite temporary filesystem: %w", err)
	}
	if same {
		required = compactByteSum(required, estimate.SQLiteTempRequiredBytes)
	}
	if err := requireCompactFilesystemSpace("index-consolidation staging filesystem", stagingDir, required); err != nil {
		return err
	}
	return requireCompactFilesystemSpace("SQLite temporary filesystem ("+estimate.SQLiteTempDir+")", estimate.SQLiteTempDir, estimate.SQLiteTempRequiredBytes)
}

func messageIndexStagingBytes(occupied int64) int64 {
	base := compactByteSum(occupied, occupied, occupied, occupied, occupied, occupied)
	return compactByteSum(base, compactSafetyBytes(base))
}

// EstimateCompactWithOptions includes the additional conservative staging
// budget for a table conversion. Reclaim estimates still cover free pages
// and WAL only; the result measures actual total file savings.
func (db *DB) EstimateCompactWithOptions(ctx context.Context, options CompactOptions) (CompactEstimate, error) {
	estimate, err := db.EstimateCompact(ctx)
	if err != nil {
		return estimate, err
	}
	if options.ConsolidateMessageIndex {
		plan, err := planMessageIndexConsolidation(ctx, db.rawReader())
		if err != nil {
			return estimate, err
		}
		if !plan.changed {
			return estimate, nil
		}

		var configured string
		err = db.rawReader().QueryRowContext(ctx, "PRAGMA temp_store_directory").Scan(&configured)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return estimate, fmt.Errorf("read SQLite temporary directory: %w", err)
		}
		estimate.SQLiteTempDir, err = messageIndexTempDirectory(ctx, configured)
		if err != nil {
			return estimate, err
		}
		tempBase := compactByteSum(estimate.EstimatedDatabaseBytes, estimate.EstimatedDatabaseBytes)
		estimate.SQLiteTempRequiredBytes = compactByteSum(tempBase, compactSafetyBytes(tempBase))
		// The displayed one-filesystem budget includes both independent work
		// areas. Admission checks split them when staging and temp differ.
		estimate.StagingRequiredBytes = max(estimate.StagingRequiredBytes, compactByteSum(messageIndexStagingBytes(estimate.EstimatedDatabaseBytes), estimate.SQLiteTempRequiredBytes))
	}
	return estimate, nil
}

// OpenForMessageIndexConsolidation avoids normal writable initialization:
// physical maintenance must not run unrelated migrations or create history.
// The caller owns the archive's existing write-owner lock.
func OpenForMessageIndexConsolidation(ctx context.Context, path string) (*DB, error) {
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
		return fail(errors.New("index consolidation requires a current archive; finish its normal upgrade first"))
	}
	if _, err := planMessageIndexConsolidation(ctx, d.rawReader()); err != nil {
		return fail(err)
	}
	writer, err := sql.Open(sqliteArchiveDriverName, makeDSN(path, false)+"&mode=rw")
	if err != nil {
		return fail(err)
	}
	writer.SetMaxOpenConns(1)
	if err := configureWALContext(ctx, writer); err != nil {
		writer.Close()
		return fail(err)
	}
	d.readOnly = false
	d.activityKept = newActivityKeptMemo()
	d.sessionStatsFacts = newSessionStatsFactsMemo()
	d.writer.Store(writer)
	return d, nil
}

// planMessageIndexConsolidation accepts only our known message definitions.
// Legacy ADD COLUMN order is permitted; unknown columns, constraints, table
// options, and message triggers fail closed. Other schema objects are retained
// verbatim, including their names and original SQL.
func planMessageIndexConsolidation(ctx context.Context, conn *sql.DB) (messageIndexPlan, error) {
	var plan messageIndexPlan
	if err := validateMessageIndexCJKState(ctx, conn); err != nil {
		return plan, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT type,name,tbl_name,COALESCE(sql,'') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`)
	if err != nil {
		return plan, err
	}
	for rows.Next() {
		var o messageIndexSchemaObject
		if err := rows.Scan(&o.typ, &o.name, &o.table, &o.ddl); err != nil {
			rows.Close()
			return plan, err
		}
		plan.objects = append(plan.objects, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return plan, err
	}
	rows.Close()
	var tableSQL, indexSQL string
	for _, o := range plan.objects {
		if o.name == messageIndexStageTable {
			return plan, errors.New("message-index staging table already exists")
		}
		if o.name == "messages" && o.typ == "table" {
			tableSQL = o.ddl
		}
		if o.name == "idx_messages_session_ordinal" && o.typ == "index" {
			indexSQL = o.ddl
		}
		if o.typ == "trigger" && o.table == "messages" && !knownMessageIndexTrigger(o.name, o.ddl) {
			return plan, fmt.Errorf("unsupported messages trigger %q", o.name)
		}
	}
	start := strings.Index(schemaSQL, "CREATE TABLE IF NOT EXISTS messages (")
	if start < 0 {
		return plan, errors.New("canonical messages schema missing")
	}
	end := strings.Index(schemaSQL[start:], "\n);")
	if end < 0 {
		return plan, errors.New("canonical messages schema incomplete")
	}
	canonical, err := messageTableClauses(schemaSQL[start : start+end+3])
	if err != nil {
		return plan, err
	}
	wanted := make(map[string]string, len(canonical))
	for _, clause := range canonical {
		wanted[strings.Fields(clause)[0]] = normalizedMessageDDL(clause)
	}
	clauses, err := messageTableClauses(tableSQL)
	if err != nil {
		return plan, err
	}
	kept := make([]string, 0, len(canonical))
	uniqueCount := 0
	for _, clause := range clauses {
		normalized := normalizedMessageDDL(clause)
		if strings.ReplaceAll(normalized, " ", "") == "unique(session_id,ordinal)" {
			uniqueCount++
			continue
		}
		parts := strings.Fields(clause)
		if len(parts) == 0 {
			return plan, errors.New("empty messages definition")
		}
		name := parts[0]
		if want, ok := wanted[name]; !ok || normalized != want {
			return plan, fmt.Errorf("unsupported messages definition %q", name)
		}
		delete(wanted, name)
		plan.columns = append(plan.columns, name)
		kept = append(kept, clause)
	}
	if len(wanted) != 0 || uniqueCount > 1 {
		return plan, errors.New("unsupported messages columns or uniqueness constraints")
	}
	oldIndex := normalizedMessageDDL(`CREATE INDEX idx_messages_session_ordinal ON messages(session_id, ordinal)`)
	newIndex := normalizedMessageDDL(messageOrdinalIndexSQL)
	switch normalizedMessageDDL(indexSQL) {
	case oldIndex:
		if uniqueCount != 1 {
			return plan, errors.New("messages has no supported uniqueness constraint")
		}
	case newIndex:
	default:
		return plan, errors.New("unsupported named message-order index")
	}
	plan.changed = uniqueCount == 1
	plan.tableDDL = `CREATE TABLE "messages" (` + strings.Join(kept, ",\n") + `)`
	hash := sha256.New()
	for _, o := range plan.objects {
		ddl := o.ddl
		if plan.changed {
			if o.typ == "table" && o.name == "messages" {
				ddl = plan.tableDDL
			}
			if o.typ == "index" && o.name == "idx_messages_session_ordinal" {
				ddl = messageOrdinalIndexSQL
			}
		}
		fmt.Fprintf(hash, "%s\x00%s\x00%s\n", o.typ, o.name, ddl)
	}
	plan.expectedHash = hex.EncodeToString(hash.Sum(nil))
	return plan, nil
}

func normalizedMessageDDL(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), ";")
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	return strings.ReplaceAll(s, "if not exists ", "")
}

// These definitions contain only literal defaults and column/UNIQUE clauses.
// This is deliberately not a general SQL rewriter: quoted identifiers,
// comments, CHECK constraints, and unfamiliar suffixes are refused.
func messageTableClauses(ddl string) ([]string, error) {
	open, close := strings.IndexByte(ddl, '('), strings.LastIndexByte(ddl, ')')
	if open < 0 || close <= open || strings.Trim(strings.TrimSpace(ddl[close+1:]), ";") != "" {
		return nil, errors.New("unsupported messages table SQL")
	}
	body := ddl[open+1 : close]
	var out []string
	depth, start := 0, 0
	quoted := false
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\'':
			if quoted && i+1 < len(body) && body[i+1] == '\'' {
				i++
				continue
			}
			quoted = !quoted
		case '"', '`', '[', ';', '-', '/':
			if !quoted {
				return nil, errors.New("unsupported messages table syntax")
			}
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
				if depth < 0 {
					return nil, errors.New("unbalanced messages table SQL")
				}
			}
		case ',':
			if !quoted && depth == 0 {
				out = append(out, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	if quoted || depth != 0 {
		return nil, errors.New("unbalanced messages table SQL")
	}
	out = append(out, strings.TrimSpace(body[start:]))
	return out, nil
}

var messageIndexTriggerPattern = regexp.MustCompile(`(?is)CREATE TRIGGER (?:IF NOT EXISTS )?([a-z_]+)\b.*?\bEND;`)

func knownMessageIndexTrigger(name, ddl string) bool {
	for _, script := range []string{schemaSQL, schemaFTS, schemaSubstringIndex, usageGenerationSchemaSQL} {
		for _, match := range messageIndexTriggerPattern.FindAllStringSubmatch(script, -1) {
			if match[1] == name {
				return normalizedMessageDDL(match[0]) == normalizedMessageDDL(ddl)
			}
		}
	}
	return false
}

func consolidateCandidateMessageIndex(ctx context.Context, path string) (hash string, changed bool, err error) {
	// This connection has no connection-local CJK write triggers. The source
	// is already write-barred and this file is a disposable, verified copy.
	conn, err := sql.Open(sqliteArchiveDriverName, makeDSN(path, false)+"&mode=rw")
	if err != nil {
		return "", false, err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	plan, err := planMessageIndexConsolidation(ctx, conn)
	if err != nil {
		return "", false, err
	}
	if !plan.changed {
		return plan.expectedHash, false, nil
	}
	pinned, err := conn.Conn(ctx)
	if err != nil {
		return "", false, err
	}
	defer pinned.Close()
	for _, statement := range []string{"PRAGMA foreign_keys=OFF", "PRAGMA journal_mode=DELETE", "PRAGMA temp_store=FILE"} {
		if _, err := pinned.ExecContext(ctx, statement); err != nil {
			return "", false, err
		}
	}
	var foreignKeys int
	if err := pinned.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return "", false, err
	}
	if foreignKeys != 0 {
		return "", false, errors.New("candidate foreign keys remain enabled")
	}
	tx, err := pinned.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := rewriteCandidateMessages(ctx, tx, plan); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	if _, err := pinned.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return "", false, err
	}
	if err := verifyMessageIndexForeignKeys(ctx, pinned); err != nil {
		return "", false, err
	}
	if err := pinned.Close(); err != nil {
		return "", false, err
	}
	actual, err := schemaHash(ctx, conn)
	if err != nil {
		return "", false, err
	}
	if actual != plan.expectedHash {
		return "", false, errors.New("consolidation changed unexpected schema objects")
	}
	// A final copy removes the pages freed by the replaced table and tree.
	// VACUUM INTO keeps its temporary output on the preflighted filesystem.
	packed := path + ".consolidated"
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			err = errors.Join(err, removeIfExists(packed+suffix))
		}
		err = errors.Join(err, removeIfExists(path+".failed"))
	}()
	if _, err := conn.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(packed, "'", "''")+"'"); err != nil {
		return "", false, err
	}
	if messageIndexVacuumTestHook != nil {
		if err := messageIndexVacuumTestHook(packed); err != nil {
			return "", false, err
		}
	}
	if err := conn.Close(); err != nil {
		return "", false, err
	}
	if err := replaceInstalledFile(packed, path); err != nil {
		return "", false, err
	}
	// The ordinary install path expects WAL mode without remaining WAL frames.
	wal, err := sql.Open(sqliteArchiveDriverName, makeDSN(path, false)+"&mode=rw")
	if err != nil {
		return "", false, err
	}
	wal.SetMaxOpenConns(1)
	if _, err := wal.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		wal.Close()
		return "", false, err
	}
	if err := wal.Close(); err != nil {
		return "", false, err
	}
	if size, err := optionalFileSize(path + "-wal"); err != nil {
		return "", false, err
	} else if size != 0 {
		return "", false, fmt.Errorf("consolidated candidate has an uncheckpointed WAL (%d bytes)", size)
	}
	if err := removeIfExists(path + "-wal"); err != nil {
		return "", false, err
	}
	if err := removeIfExists(path + "-shm"); err != nil {
		return "", false, err
	}
	return plan.expectedHash, true, nil
}

func rewriteCandidateMessages(ctx context.Context, tx *sql.Tx, plan messageIndexPlan) error {
	// Drop/recreate views and triggers in the same transaction. Leaving a view
	// that references the briefly absent table makes ALTER TABLE reject an
	// otherwise correct replacement. Their SQL is restored verbatim and the
	// final schema fingerprint permits only our two intended edits.
	for _, o := range plan.objects {
		if o.typ == "view" || o.typ == "trigger" {
			if _, err := tx.ExecContext(ctx, "DROP "+strings.ToUpper(o.typ)+" "+quoteMessageSchemaName(o.name)); err != nil {
				return err
			}
		}
	}
	ddl := strings.Replace(plan.tableDDL, `"messages"`, quoteMessageSchemaName(messageIndexStageTable), 1)
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return err
	}
	columns := strings.Join(plan.columns, ",")
	// SQLite copies values directly; Go never materializes message payloads.
	// The candidate transaction is atomic but its disk work is proportional
	// to the message table, not a fixed-size WAL/transaction guarantee.
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+messageIndexStageTable+" ("+columns+") SELECT "+columns+" FROM messages"); err != nil {
		return err
	}
	if messageIndexCopyTestHook != nil {
		if err := messageIndexCopyTestHook(ctx, tx); err != nil {
			return err
		}
	}
	var mismatch bool
	comparisons := make([]string, 0, len(plan.columns)*2)
	for _, col := range plan.columns {
		comparisons = append(comparisons, "typeof(o."+col+") IS NOT typeof(n."+col+")", "o."+col+" IS NOT n."+col+" COLLATE BINARY")
	}
	query := "SELECT (SELECT count(*) FROM messages)!=(SELECT count(*) FROM " + messageIndexStageTable + ") OR EXISTS(SELECT 1 FROM messages o LEFT JOIN " + messageIndexStageTable + " n ON n.id=o.id WHERE n.id IS NULL OR " + strings.Join(comparisons, " OR ") + ")"
	if err := tx.QueryRowContext(ctx, query).Scan(&mismatch); err != nil {
		return err
	}
	if mismatch {
		return errors.New("message values or SQLite storage types changed during consolidation")
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE messages"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+messageIndexStageTable+" RENAME TO messages"); err != nil {
		return err
	}
	for _, o := range plan.objects {
		if o.typ == "index" && o.table == "messages" {
			ddl := o.ddl
			if o.name == "idx_messages_session_ordinal" {
				ddl = messageOrdinalIndexSQL
			}
			if _, err := tx.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}
	}
	// DROP TABLE removes planner statistics for every message index. Refresh
	// only this table so the installed archive does not wait for a later
	// normal startup to regain useful plans. SQLite regenerates STAT4 samples
	// when supported instead of copying them across index representations.
	if _, err := tx.ExecContext(ctx, "ANALYZE messages"); err != nil {
		return err
	}
	for _, typ := range []string{"view", "trigger"} {
		for _, o := range plan.objects {
			if o.typ == typ {
				if _, err := tx.ExecContext(ctx, o.ddl); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func quoteMessageSchemaName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func verifyMessageIndexForeignKeys(ctx context.Context, conn interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) error {
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign-key violation after message-index consolidation")
	}
	return rows.Err()
}

// Reopening a writer normally reconciles the optional CJK corpus. Physical
// maintenance must not silently rebuild or remove it (including on rollback).
func validateMessageIndexCJKState(ctx context.Context, conn *sql.DB) error {
	var tableExists, pendingExists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='messages_cjk_fts'),EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='messages_cjk_fts_pending_sessions')`).Scan(&tableExists, &pendingExists); err != nil {
		return err
	}
	refuse := func() error {
		return errors.New("message-index consolidation requires current CJK search state and its matching tokenizer runtime; finish normal archive initialization first")
	}
	var fingerprint string
	err := conn.QueryRowContext(ctx, `SELECT CAST(value AS TEXT) FROM stats WHERE key=?`, cjkFTSFingerprintStatsKey).Scan(&fingerprint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if simpleFTSRuntimeConfig.available() {
		if !tableExists || !pendingExists || err != nil || fingerprint != simpleFTSRuntimeConfig.fingerprint {
			return refuse()
		}
	} else if tableExists || err == nil {
		return refuse()
	}
	if pendingExists {
		var pending bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages_cjk_fts_pending_sessions)`).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return refuse()
		}
	}
	if tableExists {
		var ddl string
		if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='messages_cjk_fts'`).Scan(&ddl); err != nil {
			return err
		}
		if normalizedMessageDDL(ddl) != normalizedMessageDDL(schemaCJKFTS) {
			return refuse()
		}
	}
	// ensureCJKFTS drops legacy persistent AFTER triggers and recreates these
	// BEFORE triggers. Require the already-current definitions before it runs.
	rows, err := conn.QueryContext(ctx, `SELECT name,sql FROM sqlite_master WHERE type='trigger' AND (name LIKE 'messages_cjk_%' OR name LIKE 'sessions_cjk_pending_%')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	wanted := map[string]string{}
	if pendingExists {
		for _, match := range messageIndexTriggerPattern.FindAllStringSubmatch(schemaCJKFTSPendingSessions, -1) {
			wanted[match[1]] = normalizedMessageDDL(match[0])
		}
	}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return err
		}
		expected, ok := wanted[name]
		if !ok || normalizedMessageDDL(ddl) != expected {
			return refuse()
		}
		delete(wanted, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(wanted) != 0 {
		return refuse()
	}
	return nil
}
