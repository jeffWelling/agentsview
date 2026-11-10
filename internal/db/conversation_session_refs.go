package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const conversationSessionRefsKey = "conversation_session_refs"
const conversationRefsActiveSQL = `SELECT EXISTS(SELECT 1 FROM archive_metadata WHERE key='conversation_session_refs' AND value='v1')`
const conversationPhysicalColumns = `session_id,message_id,ordinal,role,timestamp,source_id,body,digest,text_bytes,visible_digest,visible_text_bytes,visible_gap,gap,deleted,removed,revision`
const conversationMessageSchemaSQL = `CREATE TABLE IF NOT EXISTS conversation_messages (
    session_id TEXT NOT NULL, message_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL, role TEXT NOT NULL, timestamp TEXT NOT NULL DEFAULT '',
    source_id TEXT NOT NULL DEFAULT '', body TEXT, digest TEXT NOT NULL DEFAULT '',
    text_bytes INTEGER NOT NULL DEFAULT 0,
    visible_digest TEXT NOT NULL DEFAULT '',
    visible_text_bytes INTEGER NOT NULL DEFAULT 0,
    visible_gap TEXT NOT NULL DEFAULT 'visible_text_unavailable', gap TEXT NOT NULL DEFAULT '',
    deleted INTEGER NOT NULL DEFAULT 0, removed INTEGER NOT NULL DEFAULT 0,
    revision INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(session_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_conversation_messages_revision ON conversation_messages(revision);
CREATE INDEX IF NOT EXISTS idx_conversation_messages_source ON conversation_messages(session_id, source_id);
CREATE INDEX IF NOT EXISTS idx_conversation_messages_ordinal
    ON conversation_messages(session_id, ordinal) WHERE removed = 0;
`

func conversationPhysicalTableTx(ctx context.Context, tx *sql.Tx) (string, error) {
	var active bool
	if err := tx.QueryRowContext(ctx, conversationRefsActiveSQL).Scan(&active); err != nil {
		return "", err
	}
	if active {
		return "conversation_message_refs", nil
	}
	return "conversation_messages", nil
}

func conversationMessageDDL(table, mapping string, indexes bool) string {
	ddl := conversationMessageSchemaSQL
	if !indexes {
		ddl = ddl[:strings.Index(ddl, "CREATE INDEX")]
	}
	ddl = strings.ReplaceAll(ddl, "conversation_messages", table)
	if mapping != "" {
		ddl = strings.ReplaceAll(ddl, "session_id", "session_ref")
		ddl = strings.Replace(ddl, "session_ref TEXT NOT NULL", "session_ref INTEGER NOT NULL REFERENCES "+mapping+"(session_ref)", 1)
	}
	return ddl
}

func conversationRefsViewSQL(table, mapping string) string {
	return `SELECT r.session_id,m.` + strings.ReplaceAll(strings.TrimPrefix(conversationPhysicalColumns, "session_id,"), ",", ",m.") + ` FROM ` + table + ` m JOIN ` + mapping + ` r ON r.session_ref=m.session_ref`
}

func ensureConversationMessageLayoutTx(ctx context.Context, tx *sql.Tx) error {
	table, err := conversationPhysicalTableTx(ctx, tx)
	if err != nil {
		return err
	}
	if table == "conversation_messages" {
		_, err = tx.ExecContext(ctx, conversationMessageSchemaSQL)
		return err
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='conversation_message_refs') AND EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='conversation_session_refs') AND EXISTS(SELECT 1 FROM sqlite_master WHERE type='view' AND name='conversation_messages')`).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errors.New("incomplete conversation session-reference layout")
	}
	_, err = tx.ExecContext(ctx, conversationMessageDDL("conversation_message_refs", "conversation_session_refs", true))
	return err
}

// The write builder accepts clauses separately: it never parses or rewrites
// arbitrary SQL. The transaction adapter carries its caller's context.
type conversationWriteQueries struct{ transactionQueries }

func (tx conversationWriteQueries) Update(setClause, predicate string, args ...any) (sql.Result, error) {
	table, where, err := tx.target(predicate)
	if err != nil {
		return nil, err
	}
	return tx.Exec("UPDATE "+table+" SET "+setClause+" WHERE "+where, args...)
}
func (tx conversationWriteQueries) Delete(predicate string, args ...any) (sql.Result, error) {
	table, where, err := tx.target(predicate)
	if err != nil {
		return nil, err
	}
	return tx.Exec("DELETE FROM "+table+" WHERE "+where, args...)
}
func (tx conversationWriteQueries) target(predicate string) (string, string, error) {
	var refs bool
	if err := tx.QueryRow(conversationRefsActiveSQL).Scan(&refs); err != nil {
		return "", "", err
	}
	if !refs {
		return "conversation_messages", predicate, nil
	}
	where := `(session_ref,message_id) IN (SELECT (SELECT session_ref FROM conversation_session_refs r WHERE r.session_id=c.session_id),c.message_id FROM conversation_messages c WHERE ` + predicate + `)`
	return "conversation_message_refs", where, nil
}

// Transform only physical message trigger statements. Session-level journals
// and logical predicates retain their public text session identities.
func conversationSessionRefTriggerSQL(ddl string, enabled bool) string {
	if !enabled {
		return ddl
	}
	ddl = strings.ReplaceAll(ddl, "ON conversation_messages\n", "ON conversation_message_refs\n")
	ddl = strings.ReplaceAll(ddl, "UPDATE conversation_messages SET revision=", "UPDATE conversation_message_refs SET revision=")
	ddl = strings.ReplaceAll(ddl, "WHERE session_id=NEW.session_id AND message_id=NEW.message_id", "WHERE session_ref=NEW.session_ref AND message_id=NEW.message_id")
	ddl = strings.ReplaceAll(ddl, "UPDATE conversation_messages SET deleted=1,removed=1,body=NULL WHERE session_id=OLD.id", `UPDATE conversation_message_refs SET deleted=1,removed=1,body=NULL WHERE session_ref=(SELECT session_ref FROM conversation_session_refs WHERE session_id=OLD.id)`)
	ddl = strings.ReplaceAll(ddl, "UPDATE conversation_messages SET deleted=(NEW.deleted_at IS NOT NULL) WHERE session_id=NEW.id", `UPDATE conversation_message_refs SET deleted=(NEW.deleted_at IS NOT NULL) WHERE session_ref=(SELECT session_ref FROM conversation_session_refs WHERE session_id=NEW.id)`)
	return ddl
}

func insertConversationSelectTx(ctx context.Context, tx *sql.Tx, columns, selection string, ignore bool, args ...any) error {
	table, err := conversationPhysicalTableTx(ctx, tx)
	if err != nil {
		return err
	}
	prefix := "INSERT INTO "
	if ignore {
		prefix = "INSERT OR IGNORE INTO "
	}
	if table != "conversation_messages" {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_session_refs(session_id) SELECT session_id FROM (`+selection+`)`, args...); err != nil {
			return err
		}
		selection = `SELECT (SELECT session_ref FROM conversation_session_refs r WHERE r.session_id=c.session_id),c.` + strings.ReplaceAll(strings.TrimPrefix(columns, "session_id,"), ",", ",c.") + ` FROM (` + selection + `) c`
		columns = strings.Replace(columns, "session_id", "session_ref", 1)
	}
	_, err = tx.ExecContext(ctx, prefix+table+" ("+columns+") "+selection, args...)
	return err
}

// ConversationSessionRefsReport describes representation changes, not logical
// transcript changes. File-space reclamation remains a separate db compact.
type ConversationSessionRefsReport struct {
	Rows    int64 `json:"rows"`
	Enabled bool  `json:"enabled"`
	Changed bool  `json:"changed"`
}

// SetConversationSessionRefs stages bounded copies while holding this handle's
// writer mutex, verifies every logical field, then swaps the representation in
// one transaction. The caller must own the archive's process-level write lock
// and close other direct clients. Cancellation before cutover leaves the
// original authoritative representation intact; a retry restarts staging.
func (db *DB) SetConversationSessionRefs(ctx context.Context, enabled bool) (ConversationSessionRefsReport, error) {
	report := ConversationSessionRefsReport{Enabled: enabled}
	if db.readOnly {
		return report, ErrReadOnly
	}
	if err := db.lockWriterCtx(ctx); err != nil {
		return report, err
	}
	defer db.mu.Unlock()
	if db.writerClosed.Load() {
		return report, ErrWriterClosed
	}
	var active bool
	if err := db.getWriter().QueryRowContext(ctx, conversationRefsActiveSQL).Scan(&active); err != nil {
		return report, err
	}
	if active == enabled {
		return report, nil
	}
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	if err = validateConversationRefLayoutTx(ctx, tx, active); err != nil {
		_ = tx.Rollback()
		return report, err
	}
	if err = prepareConversationRefStageTx(ctx, tx, enabled); err != nil {
		_ = tx.Rollback()
		return report, err
	}
	if err = tx.Commit(); err != nil {
		return report, err
	}
	source := "conversation_messages"
	if active {
		source = "conversation_message_refs"
	}
	var after any
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		tx, err = db.getWriter().BeginTx(ctx, nil)
		if err != nil {
			return report, err
		}
		var through sql.NullInt64
		boundarySQL := conversationRefBatchBoundarySQL(source, after != nil)
		var boundaryArgs []any
		if after != nil {
			boundaryArgs = []any{after}
		}
		err = tx.QueryRowContext(ctx, boundarySQL, boundaryArgs...).Scan(&through)
		if err == nil && through.Valid {
			err = copyConversationRefBatchTx(ctx, tx, enabled, after, through.Int64)
		}
		if err != nil {
			_ = tx.Rollback()
			return report, err
		}
		if err = tx.Commit(); err != nil {
			return report, err
		}
		if !through.Valid {
			break
		}
		after = through.Int64
	}
	tx, err = db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = verifyConversationRefStageTx(ctx, tx, enabled); err != nil {
		return report, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_messages`).Scan(&report.Rows); err != nil {
		return report, err
	}
	if err = finishConversationRefStageTx(ctx, tx, enabled); err != nil {
		return report, err
	}
	if err = tx.Commit(); err != nil {
		return report, err
	}
	report.Changed = true
	return report, nil
}

// Explicit copy and conversion boundaries validate physical history. This scan
// must not run on ordinary startup or once per copied session.
func validateConversationRefRows(ctx context.Context, q archiveFormatQuerier, schema string) error {
	var missing bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+schema+`.conversation_message_refs m LEFT JOIN `+schema+`.conversation_session_refs r ON r.session_ref=m.session_ref WHERE r.session_ref IS NULL)`).Scan(&missing); err != nil {
		return err
	}
	if missing {
		return errors.New("conversation session-reference rows are missing their session mapping; conversion or copy refused")
	}
	return nil
}

func validateAttachedConversationSource(ctx context.Context, q archiveFormatQuerier, schema string) error {
	if err := validateAttachedArchiveFormat(ctx, q, schema); err != nil {
		return err
	}
	flags, err := archiveFormatFlagsInSchema(ctx, q, schema)
	if err != nil || flags&archiveFormatSessionRefs == 0 {
		return err
	}
	return validateConversationRefRows(ctx, q, schema)
}

// Conversion replaces the physical table. Refuse unrecognized columns or
// attached schema objects rather than silently discarding extensions at cutover.
func validateConversationRefLayoutTx(ctx context.Context, tx *sql.Tx, active bool) error {
	table, columns := "conversation_messages", conversationPhysicalColumns
	if active {
		table = "conversation_message_refs"
		columns = strings.Replace(columns, "session_id", "session_ref", 1)
		if err := validateConversationRefRows(ctx, tx, "main"); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return err
	}
	var actual []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		actual = append(actual, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	expected := strings.Split(columns, ",")
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		return errors.New("conversation format conversion requires the supported 16-column layout")
	}
	allowed := map[string]bool{}
	for _, suffix := range []string{"revision", "source", "ordinal"} {
		allowed["idx_"+table+"_"+suffix] = true
	}
	for _, match := range conversationTriggerNames.FindAllStringSubmatch(conversationSchemaSQL+conversationVisibleRevisionSQL, -1) {
		allowed[match[1]] = true
	}
	for _, name := range conversationDigestTriggerNames {
		allowed[name] = true
	}
	rows, err = tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE tbl_name=? AND type IN ('index','trigger')`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if !allowed[name] && !strings.HasPrefix(name, "sqlite_autoindex_"+table+"_") {
			return fmt.Errorf("conversation format conversion refuses unrecognized schema object %q", name)
		}
	}
	return rows.Err()
}

func prepareConversationRefStageTx(ctx context.Context, tx *sql.Tx, enabled bool) error {
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS _conversation_messages_stage; DROP TABLE IF EXISTS _conversation_session_refs_stage`); err != nil {
		return err
	}
	mapping := ""
	if enabled {
		mapping = "_conversation_session_refs_stage"
		if _, err := tx.ExecContext(ctx, `CREATE TABLE _conversation_session_refs_stage(session_ref INTEGER PRIMARY KEY,session_id TEXT NOT NULL UNIQUE)`); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, conversationMessageDDL("_conversation_messages_stage", mapping, false))
	return err
}

func conversationRefBatchBoundarySQL(source string, continuation bool) string {
	where := ""
	if continuation {
		where = " WHERE rowid>?"
	}
	return `SELECT MAX(rowid) FROM (SELECT rowid FROM ` + source + where + ` ORDER BY rowid LIMIT 512)`
}

func copyConversationRefBatchTx(ctx context.Context, tx *sql.Tx, enabled bool, after any, through int64) error {
	columns := conversationPhysicalColumns
	predicate, args := "rowid<=?", []any{through}
	if after != nil {
		predicate, args = "rowid>? AND rowid<=?", []any{after, through}
	}
	selection := `SELECT ` + columns + ` FROM conversation_messages WHERE ` + predicate
	if enabled {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO _conversation_session_refs_stage(session_id) SELECT session_id FROM conversation_messages WHERE `+predicate, args...); err != nil {
			return err
		}
		selection = `SELECT r.session_ref,m.` + strings.ReplaceAll(strings.TrimPrefix(columns, "session_id,"), ",", ",m.") + ` FROM conversation_messages m JOIN _conversation_session_refs_stage r ON r.session_id=m.session_id WHERE ` + strings.ReplaceAll(predicate, "rowid", "m.rowid")
		columns = strings.Replace(columns, "session_id", "session_ref", 1)
	} else {
		selection = conversationRefsViewSQL("conversation_message_refs", "conversation_session_refs") + ` WHERE ` + strings.ReplaceAll(predicate, "rowid", "m.rowid")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO _conversation_messages_stage (`+columns+`) `+selection, args...)
	return err
}

func verifyConversationRefStageTx(ctx context.Context, tx *sql.Tx, enabled bool) error {
	stage := `SELECT ` + conversationPhysicalColumns + ` FROM _conversation_messages_stage`
	if enabled {
		stage = conversationRefsViewSQL("_conversation_messages_stage", "_conversation_session_refs_stage")
	}
	original := `SELECT ` + conversationPhysicalColumns + ` FROM conversation_messages`
	for _, pair := range [][2]string{{original, stage}, {stage, original}} {
		var differs bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(`+pair[0]+` EXCEPT `+pair[1]+`)`).Scan(&differs); err != nil {
			return err
		}
		if differs {
			return errors.New("conversation format copy differs from the original; cutover refused")
		}
	}
	return nil
}

var conversationTriggerNames = regexp.MustCompile(`CREATE TRIGGER IF NOT EXISTS ([a-z_]+)`)

func finishConversationRefStageTx(ctx context.Context, tx *sql.Tx, enabled bool) error {
	version, _, err := readUserVersion(ctx, tx)
	if err != nil {
		return err
	}
	for _, match := range conversationTriggerNames.FindAllStringSubmatch(conversationSchemaSQL+conversationVisibleRevisionSQL, -1) {
		if _, err := tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+match[1]); err != nil {
			return err
		}
	}
	if enabled {
		if _, err := tx.ExecContext(ctx, `DROP TABLE conversation_messages; ALTER TABLE _conversation_session_refs_stage RENAME TO conversation_session_refs; ALTER TABLE _conversation_messages_stage RENAME TO conversation_message_refs`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE VIEW conversation_messages AS `+conversationRefsViewSQL("conversation_message_refs", "conversation_session_refs")); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, conversationMessageDDL("conversation_message_refs", "conversation_session_refs", true)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,'v1') ON CONFLICT(key) DO UPDATE SET value=excluded.value`, conversationSessionRefsKey); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DROP VIEW conversation_messages; DROP TABLE conversation_message_refs; DROP TABLE conversation_session_refs; ALTER TABLE _conversation_messages_stage RENAME TO conversation_messages`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, conversationMessageSchemaSQL); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key=?`, conversationSessionRefsKey); err != nil {
			return err
		}
	}
	mode, err := conversationDigestModeTx(ctx, tx)
	if err != nil {
		return err
	}
	if err := installConversationDigestTriggers(ctx, tx, mode); err != nil {
		return err
	}
	return writeUserVersion(ctx, tx, version)
}

// Rebuilds copy the policy before ingesting any rows. They have no conversation
// history yet, so applying it never starts an archive-sized startup migration.
func initializeConversationRefsForRebuildTx(ctx context.Context, tx *sql.Tx) error {
	var refs bool
	if err := tx.QueryRowContext(ctx, conversationRefsActiveSQL).Scan(&refs); err != nil || !refs {
		return err
	}
	var view bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='view' AND name='conversation_messages')`).Scan(&view); err != nil || view {
		return err
	}
	var populated bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_messages)`).Scan(&populated); err != nil {
		return err
	}
	if populated {
		return fmt.Errorf("session-reference policy must be copied before conversation history")
	}
	if err := prepareConversationRefStageTx(ctx, tx, true); err != nil {
		return err
	}
	return finishConversationRefStageTx(ctx, tx, true)
}
