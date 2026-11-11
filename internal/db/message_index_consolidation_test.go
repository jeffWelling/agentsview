package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Literal pre-consolidation DDL, including both uniqueness enforcement and
// the separately named nonunique tree used by older INDEXED BY readers.
const legacyMessageIndexTable = `CREATE TABLE messages (
 id INTEGER PRIMARY KEY,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 ordinal INTEGER NOT NULL,
 role TEXT NOT NULL,
 content TEXT NOT NULL,
 thinking_text TEXT NOT NULL DEFAULT '',
 dialogue_text TEXT,
 dialogue_is_content INTEGER NOT NULL DEFAULT 0,
 timestamp TEXT,
 has_thinking INTEGER NOT NULL DEFAULT 0,
 has_tool_use INTEGER NOT NULL DEFAULT 0,
 content_length INTEGER NOT NULL DEFAULT 0,
 is_system INTEGER NOT NULL DEFAULT 0,
 model TEXT NOT NULL DEFAULT '',
 reasoning_effort TEXT NOT NULL DEFAULT '',
 token_usage TEXT NOT NULL DEFAULT '',
 context_tokens INTEGER NOT NULL DEFAULT 0,
 output_tokens INTEGER NOT NULL DEFAULT 0,
 provider_id TEXT NOT NULL DEFAULT '',
 has_context_tokens INTEGER NOT NULL DEFAULT 0,
 has_output_tokens INTEGER NOT NULL DEFAULT 0,
 claude_message_id TEXT NOT NULL DEFAULT '',
 claude_request_id TEXT NOT NULL DEFAULT '',
 source_type TEXT NOT NULL DEFAULT '',
 source_subtype TEXT NOT NULL DEFAULT '',
 prompt_source TEXT NOT NULL DEFAULT '',
 source_uuid TEXT NOT NULL DEFAULT '',
 source_parent_uuid TEXT NOT NULL DEFAULT '',
 is_sidechain INTEGER NOT NULL DEFAULT 0,
 is_compact_boundary INTEGER NOT NULL DEFAULT 0,
 UNIQUE(session_id, ordinal)
)`

func legacyMessageIndexDB(t testing.TB) *DB {
	return legacyMessageIndexDBWithDDL(t, legacyMessageIndexTable)
}

func legacyMessageIndexDBWithDDL(t testing.TB, ddl string) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.db")
	raw, err := sql.Open(sqliteArchiveDriverName, makeDSN(path, false))
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), ddl+`; CREATE INDEX idx_messages_session_ordinal ON messages(session_id, ordinal);`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	var implicit, named int
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT sum(origin='u'), sum(name='idx_messages_session_ordinal' AND "unique"=0) FROM pragma_index_list('messages')`).Scan(&implicit, &named))
	require.Equal(t, 1, implicit)
	require.Equal(t, 1, named)
	return d
}

func seedMessageIndexHistory(t *testing.T, d *DB) {
	t.Helper()
	insertSession(t, d, "saved-session", "project")
	insertSession(t, d, "other-session", "project")
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO messages(id,session_id,ordinal,role,content,thinking_text,dialogue_text,timestamp,source_uuid,token_usage,model) VALUES
		 (-7,'other-session',0,'user','negative row survives','','',NULL,'','', ''),
		 (41,'saved-session',0,'user',?,'reasoning needle',NULL,'2026-01-01T00:00:00Z','opaque-message','', ''),
		 (99,'saved-session',1,'assistant','second heliotrope','','second heliotrope','','',?, 'model')`, "first heliotrope\x00雪", `{"input_tokens":7,"output_tokens":3}`)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `INSERT INTO tool_calls(id,message_id,session_id,tool_name,category,tool_use_id,input_json) VALUES(71,41,'saved-session','Read','read','opaque-call','{"path":"retained"}');
		INSERT INTO pinned_messages(id,session_id,message_id,ordinal,note) VALUES(81,'saved-session',41,0,'keep this pin');`)
		return err
	}))
	_, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{Limit: 100})
	require.NoError(t, err)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,body,digest,text_bytes,gap,visible_digest,visible_text_bytes,visible_gap,deleted,removed) VALUES('saved-session','retained-tombstone',7,'assistant','retained historical body','legacy',24,'','legacy-visible',24,'',0,1)`)
		return err
	}))
}

func messageIndexRows(t testing.TB, conn *sql.DB, table string) [][]any {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(), "SELECT * FROM "+quoteMessageSchemaName(table)+" ORDER BY 1")
	require.NoError(t, err)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out [][]any
	for rows.Next() {
		values := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(t, rows.Scan(pointers...))
		for i, v := range values {
			if b, ok := v.([]byte); ok {
				values[i] = append([]byte{}, b...)
			}
		}
		out = append(out, values)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestMessageIndexConsolidationPreservesHistoryAndOldSQL(t *testing.T) {
	d := legacyMessageIndexDB(t)
	seedMessageIndexHistory(t, d)
	tables := []string{"messages", "sessions", "tool_calls", "pinned_messages", "conversation_messages", "conversation_session_changes", "archive_metadata", "stats"}
	before := map[string][][]any{}
	for _, table := range tables {
		before[table] = messageIndexRows(t, d.rawReader(), table)
	}
	var version int
	require.NoError(t, d.Reader().QueryRow(t.Context(), "PRAGMA user_version").Scan(&version))
	result, err := d.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir(), KeepBackup: true, ConsolidateMessageIndex: true})
	require.NoError(t, err)
	assert.Equal(t, "consolidated", result.MessageIndexStatus)
	for _, table := range tables {
		assert.Equal(t, before[table], messageIndexRows(t, d.rawReader(), table), table)
	}
	var afterVersion, unique, implicit int
	require.NoError(t, d.Reader().QueryRow(t.Context(), "PRAGMA user_version").Scan(&afterVersion))
	assert.Equal(t, version, afterVersion)
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT sum(name='idx_messages_session_ordinal' AND "unique"=1),sum(origin='u') FROM pragma_index_list('messages')`).Scan(&unique, &implicit))
	assert.Equal(t, 1, unique)
	assert.Zero(t, implicit)
	require.NoError(t, verifyMessageIndexForeignKeys(t.Context(), d.rawReader()))
	backup, err := sql.Open(sqliteArchiveDriverName, makeDSN(result.BackupPath, true))
	require.NoError(t, err)
	defer backup.Close()
	assert.Equal(t, before["messages"], messageIndexRows(t, backup, "messages"))
	var backupUnique int
	require.NoError(t, backup.QueryRowContext(t.Context(), `SELECT "unique" FROM pragma_index_list('messages') WHERE name='idx_messages_session_ordinal'`).Scan(&backupUnique))
	assert.Zero(t, backupUnique)
	// This is the older production append extraction, with its required name.
	rows, err := d.Reader().QueryContext(t.Context(), usageFillAppendFactsSQL, "saved-session", -1)
	require.NoError(t, err)
	var ordinals []int
	for rows.Next() {
		var sid, role, ts, model, provider, usage, cm, cr, source string
		var ordinal int
		require.NoError(t, rows.Scan(&sid, &ordinal, &role, &ts, &model, &provider, &usage, &cm, &cr, &source))
		ordinals = append(ordinals, ordinal)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.ElementsMatch(t, []int{0, 1}, ordinals)
	assertMessageIndexSearch(t, d, "heliotrope", []int64{41, 99})
	// Old ON CONFLICT writers keep the original physical ID, update FTS, and
	// still reject duplicate keys. Later cascade deletes retain FK support.
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content) VALUES('saved-session',0,'user','new marigold') ON CONFLICT(session_id,ordinal) DO UPDATE SET content=excluded.content`)
		return err
	}))
	assertMessageIndexSearch(t, d, "marigold", []int64{41})
	assertMessageIndexSearch(t, d, "heliotrope", []int64{99})
	err = d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content) VALUES('saved-session',0,'user','duplicate')`)
		return err
	})
	require.ErrorContains(t, err, "UNIQUE constraint failed")
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM messages WHERE id=41`)
		return err
	}))
	var children int
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM tool_calls WHERE id=71)+(SELECT count(*) FROM pinned_messages WHERE id=81)`).Scan(&children))
	assert.Zero(t, children)
	assertMessageIndexSearch(t, d, "marigold", nil)
	require.NoError(t, d.Close())
	reopened, err := OpenIsolated(t.Context(), d.Path())
	require.NoError(t, err)
	defer reopened.Close()
	again, err := reopened.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir(), ConsolidateMessageIndex: true})
	require.NoError(t, err)
	assert.Equal(t, "already_canonical", again.MessageIndexStatus)
}

func assertMessageIndexSearch(t *testing.T, d *DB, term string, want []int64) {
	t.Helper()
	rows, err := d.Reader().QueryContext(t.Context(), `SELECT rowid FROM messages_fts WHERE rendered MATCH ? ORDER BY rowid`, term)
	require.NoError(t, err)
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		got = append(got, id)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, want, got)
}

func TestMessageIndexConsolidationFreshUniqueness(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s", "p")
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content) VALUES('s',0,'user','one'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('s',0,'user','two') ON CONFLICT(session_id,ordinal) DO UPDATE SET content=excluded.content`)
		return err
	}))
	var content string
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT content FROM messages INDEXED BY idx_messages_session_ordinal WHERE session_id='s' AND ordinal=0`).Scan(&content))
	assert.Equal(t, "two", content)
	err := d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content) VALUES('s',0,'user','three')`)
		return err
	})
	require.ErrorContains(t, err, "UNIQUE constraint failed")
}

func TestMessageIndexConsolidationRejectsChangesBeforeInstall(t *testing.T) {
	for _, kind := range []string{"cancel", "disk-full", "value", "type", "unknown-column", "unknown-constraint", "unknown-trigger"} {
		t.Run(kind, func(t *testing.T) {
			ddl := legacyMessageIndexTable
			if kind == "unknown-constraint" {
				ddl = strings.Replace(ddl, "ordinal INTEGER NOT NULL,", "ordinal INTEGER NOT NULL CHECK(ordinal>=0),", 1)
			}
			d := legacyMessageIndexDBWithDDL(t, ddl)
			seedMessageIndexHistory(t, d)
			if kind == "unknown-column" {
				require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), `ALTER TABLE messages ADD COLUMN extension TEXT`)
					return err
				}))
			}
			if kind == "unknown-trigger" {
				require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER extension_trigger AFTER UPDATE ON messages BEGIN SELECT 1; END`)
					return err
				}))
			}
			before := messageIndexRows(t, d.rawReader(), "messages")
			pins := messageIndexRows(t, d.rawReader(), "pinned_messages")
			history := messageIndexRows(t, d.rawReader(), "conversation_messages")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			messageIndexCopyTestHook = func(ctx context.Context, tx *sql.Tx) error {
				switch kind {
				case "disk-full":
					var pages int
					if err := tx.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, `CREATE TABLE message_index_disk_full_probe(value BLOB); INSERT INTO message_index_disk_full_probe VALUES(zeroblob(1048576))`)
					return err
				case "cancel":
					cancel()
					return ctx.Err()
				case "value":
					_, err := tx.ExecContext(ctx, `UPDATE messages_index_consolidation SET content='corrupted' WHERE id=41`)
					return err
				case "type":
					_, err := tx.ExecContext(ctx, `UPDATE messages_index_consolidation SET content=CAST(content AS BLOB) WHERE id=41`)
					return err
				}
				return nil
			}
			defer func() { messageIndexCopyTestHook = nil }()
			_, err := d.Compact(ctx, CompactOptions{StagingDir: t.TempDir(), ConsolidateMessageIndex: true})
			require.Error(t, err)
			if kind == "cancel" {
				require.ErrorIs(t, err, context.Canceled)
			}
			if kind == "disk-full" {
				require.ErrorContains(t, err, "database or disk is full")
			}
			assert.Equal(t, before, messageIndexRows(t, d.rawReader(), "messages"))
			assert.Equal(t, pins, messageIndexRows(t, d.rawReader(), "pinned_messages"))
			assert.Equal(t, history, messageIndexRows(t, d.rawReader(), "conversation_messages"))
			assert.False(t, d.WriterClosed())
			assert.NoFileExists(t, compactManifestPath(d.Path()))
		})
	}
}

func TestMessageIndexConsolidationRecoveryRestoresDifferentOriginalSchema(t *testing.T) {
	d := legacyMessageIndexDB(t)
	seedMessageIndexHistory(t, d)
	expected, err := d.captureVerification(t.Context())
	require.NoError(t, err)
	before := messageIndexRows(t, d.rawReader(), "messages")
	op, err := d.prepareCompactStaging(t.Context(), CompactOptions{StagingDir: t.TempDir(), KeepBackup: true, ConsolidateMessageIndex: true}, mustMessageCompactEstimate(t, d))
	require.NoError(t, err)
	require.NoError(t, d.CloseWriter())
	require.NoError(t, d.buildCompactCandidate(t.Context(), op))
	assert.NotEqual(t, expected.SchemaHash, op.manifest.ExpectedSchemaHash)
	assert.Equal(t, expected.SchemaHash, op.manifest.OriginalSchemaHash)
	require.NoError(t, d.Close())
	// The candidate and installed path are unusable; only the intact original
	// backup can recover. Version 1's single schema hash cannot do this.
	require.NoError(t, os.Remove(d.Path()))
	require.NoError(t, os.WriteFile(op.candidatePath, []byte("invalid candidate"), 0o600))
	require.NoError(t, RecoverCompactManifest(d.Path()))
	recovered, err := sql.Open(sqliteArchiveDriverName, makeDSN(d.Path(), true))
	require.NoError(t, err)
	defer recovered.Close()
	assert.Equal(t, before, messageIndexRows(t, recovered, "messages"))
	hash, err := schemaHash(t.Context(), recovered)
	require.NoError(t, err)
	assert.Equal(t, expected.SchemaHash, hash)
}

func mustMessageCompactEstimate(t *testing.T, d *DB) CompactEstimate {
	t.Helper()
	estimate, err := d.EstimateCompactWithOptions(t.Context(), CompactOptions{ConsolidateMessageIndex: true})
	require.NoError(t, err)
	return estimate
}

func TestMessageIndexConsolidationCleansFinalVacuumOnAbort(t *testing.T) {
	d := legacyMessageIndexDB(t)
	seedMessageIndexHistory(t, d)
	before := messageIndexRows(t, d.rawReader(), "messages")
	staging := t.TempDir()
	var output string
	messageIndexVacuumTestHook = func(path string) error { output = path; require.FileExists(t, path); return context.Canceled }
	defer func() { messageIndexVacuumTestHook = nil }()
	_, err := d.Compact(t.Context(), CompactOptions{StagingDir: staging, ConsolidateMessageIndex: true})
	require.ErrorIs(t, err, context.Canceled)
	require.NotEmpty(t, output)
	assert.NoFileExists(t, output)
	assert.NoDirExists(t, filepath.Dir(output))
	assert.Equal(t, before, messageIndexRows(t, d.rawReader(), "messages"))
	assert.False(t, d.WriterClosed())
}

func TestMessageIndexConsolidationSweepsOnlyKnownOutputs(t *testing.T) {
	staging := t.TempDir()
	prefix := compactOpDirPrefixFor(filepath.Join(t.TempDir(), "archive.db"))
	for _, unknown := range []bool{false, true} {
		dir, err := os.MkdirTemp(staging, prefix)
		require.NoError(t, err)
		for _, name := range []string{compactCandidateName, compactCandidateName + ".consolidated", compactCandidateName + ".consolidated-journal", compactCandidateName + ".failed"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("partial output"), 0o600))
		}
		if unknown {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "unrecognized"), []byte("keep"), 0o600))
		}
		sweepCompactOrphans(staging, "", prefix)
		if unknown {
			require.FileExists(t, filepath.Join(dir, "unrecognized"))
		} else {
			require.NoDirExists(t, dir)
		}
		assert.NoFileExists(t, filepath.Join(dir, compactCandidateName+".consolidated"))
	}
}

func TestMessageIndexConsolidationLegacyColumnOrderAndDependencies(t *testing.T) {
	// ALTER TABLE upgrades append columns in a different order than fresh DDL.
	ddl := strings.Replace(legacyMessageIndexTable, " thinking_text TEXT NOT NULL DEFAULT '',\n", "", 1)
	ddl = strings.Replace(ddl, " UNIQUE(session_id, ordinal)", " thinking_text TEXT NOT NULL DEFAULT '',\n UNIQUE(session_id, ordinal)", 1)
	d := legacyMessageIndexDBWithDDL(t, ddl)
	seedMessageIndexHistory(t, d)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE VIEW saved_message_view AS SELECT id,content FROM messages; CREATE TABLE extension_audit(value TEXT); CREATE TRIGGER extension_audit_insert AFTER INSERT ON extension_audit BEGIN SELECT content FROM saved_message_view LIMIT 1; END; INSERT INTO extension_audit VALUES('unchanged');`)
		return err
	}))
	before := messageIndexRows(t, d.rawReader(), "messages")
	// Ordinary maintenance must keep the existing representation until opted in.
	result, err := d.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir()})
	require.NoError(t, err)
	assert.Empty(t, result.MessageIndexStatus)
	var implicit int
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT count(*) FROM pragma_index_list('messages') WHERE origin='u'`).Scan(&implicit))
	assert.Equal(t, 1, implicit)
	result, err = d.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir(), ConsolidateMessageIndex: true})
	require.NoError(t, err)
	assert.Equal(t, "consolidated", result.MessageIndexStatus)
	assert.Equal(t, before, messageIndexRows(t, d.rawReader(), "messages"))
	var content string
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT content FROM saved_message_view WHERE id=41`).Scan(&content))
	assert.Equal(t, "first heliotrope\x00雪", content)
	var extension string
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT value FROM extension_audit`).Scan(&extension))
	assert.Equal(t, "unchanged", extension)
}

func TestMessageIndexConsolidationCJK(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	d := legacyMessageIndexDB(t)
	seedSearchSession(t, d, "chinese", "project", [][2]string{{"user", "请验证这段长句中的中文搜索功能，并检查 SQLite 集成。"}})
	require.True(t, d.HasCJKFTS(t.Context()))
	tables := []string{"messages_cjk_fts_data", "messages_cjk_fts_idx", "messages_cjk_fts_docsize", "messages_cjk_fts_config", "messages_cjk_fts_pending_sessions", "stats"}
	before := map[string][][]any{}
	for _, table := range tables {
		before[table] = messageIndexRows(t, d.rawReader(), table)
	}
	_, err := d.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir(), ConsolidateMessageIndex: true})
	require.NoError(t, err)
	for _, table := range tables {
		assert.Equal(t, before[table], messageIndexRows(t, d.rawReader(), table), table)
	}
	require.True(t, d.HasCJKFTS(t.Context()))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content) VALUES('chinese',1,'user','后来插入的中文搜索内容。'); UPDATE messages SET content='更新后的中文搜索内容。' WHERE session_id='chinese' AND ordinal=0; DELETE FROM messages WHERE session_id='chinese' AND ordinal=1; INSERT INTO messages_cjk_fts(messages_cjk_fts,rank) VALUES('integrity-check',1)`)
		return err
	}))
	var pending int
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT count(*) FROM messages_cjk_fts_pending_sessions`).Scan(&pending))
	assert.Zero(t, pending)
}

func TestMessageIndexConsolidationRefusesCJKReconciliation(t *testing.T) {
	for _, state := range []string{"fingerprint", "pending", "legacy-trigger"} {
		t.Run(state, func(t *testing.T) {
			d := legacyMessageIndexDB(t)
			seedMessageIndexHistory(t, d)
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				switch state {
				case "fingerprint":
					_, err := tx.ExecContext(t.Context(), `INSERT INTO stats(key,value) VALUES(?,'foreign-runtime') ON CONFLICT(key) DO UPDATE SET value=excluded.value`, cjkFTSFingerprintStatsKey)
					return err
				case "pending":
					if _, err := tx.ExecContext(t.Context(), schemaCJKFTSPendingSessions); err != nil {
						return err
					}
					_, err := tx.ExecContext(t.Context(), `INSERT INTO messages_cjk_fts_pending_sessions VALUES('saved-session',2)`)
					return err
				default:
					_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER sessions_cjk_pending_ai AFTER INSERT ON sessions BEGIN SELECT 1; END`)
					return err
				}
			}))
			before := messageIndexRows(t, d.rawReader(), "messages")
			hash, err := schemaHash(t.Context(), d.rawReader())
			require.NoError(t, err)
			_, err = d.Compact(t.Context(), CompactOptions{ConsolidateMessageIndex: true, StagingDir: t.TempDir()})
			require.ErrorContains(t, err, "current CJK search state")
			assert.Equal(t, before, messageIndexRows(t, d.rawReader(), "messages"))
			after, err := schemaHash(t.Context(), d.rawReader())
			require.NoError(t, err)
			assert.Equal(t, hash, after)
			require.NoError(t, d.Close())
			opened, err := OpenForMessageIndexConsolidation(t.Context(), d.Path())
			require.Error(t, err)
			require.Nil(t, opened)
		})
	}
}

func TestMessageIndexConsolidationSurvivesCancelAfterInstall(t *testing.T) {
	d := legacyMessageIndexDB(t)
	seedMessageIndexHistory(t, d)
	before := messageIndexRows(t, d.rawReader(), "messages")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	compactTestHookAfterInstall = cancel
	defer func() { compactTestHookAfterInstall = nil }()
	result, err := d.Compact(ctx, CompactOptions{StagingDir: t.TempDir(), ConsolidateMessageIndex: true})
	require.NoError(t, err)
	assert.Equal(t, "consolidated", result.MessageIndexStatus)
	assert.Equal(t, before, messageIndexRows(t, d.rawReader(), "messages"))
	assert.NoFileExists(t, compactManifestPath(d.Path()))
	assert.False(t, d.WriterClosed())
}

func TestMessageIndexConsolidationRefusesHeldReader(t *testing.T) {
	d := legacyMessageIndexDB(t)
	seedMessageIndexHistory(t, d)
	before := messageIndexRows(t, d.rawReader(), "messages")
	require.NoError(t, d.CheckpointWALTruncateWithRetry(t.Context()))
	rows, err := d.rawReader().QueryContext(t.Context(), `SELECT id FROM messages ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	restore := SetCloseDrainTimeoutForTest(20 * time.Millisecond)
	defer restore()
	staging := t.TempDir()
	_, err = d.Compact(t.Context(), CompactOptions{StagingDir: staging, ConsolidateMessageIndex: true})
	require.ErrorContains(t, err, "database file is not safe to replace")
	assert.Equal(t, before, messageIndexRows(t, d.rawReader(), "messages"))
	assert.False(t, d.WriterClosed())
	var implicit int
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT count(*) FROM pragma_index_list('messages') WHERE origin='u'`).Scan(&implicit))
	assert.Equal(t, 1, implicit)
	entries, err := os.ReadDir(staging)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.NoFileExists(t, compactManifestPath(d.Path()))
}

func TestOpenForMessageIndexConsolidationRefusesColdOrOldArchive(t *testing.T) {
	cold := filepath.Join(t.TempDir(), "absent.db")
	opened, err := OpenForMessageIndexConsolidation(t.Context(), cold)
	require.Error(t, err)
	require.Nil(t, opened)
	assert.NoFileExists(t, cold)
	d := testDB(t)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version=%d", dataVersion-1))
		return err
	}))
	path := d.Path()
	require.NoError(t, d.Close())
	opened, err = OpenForMessageIndexConsolidation(t.Context(), path)
	require.ErrorContains(t, err, "current archive")
	require.Nil(t, opened)
	raw, err := sql.Open(sqliteArchiveDriverName, makeDSN(path, true))
	require.NoError(t, err)
	defer raw.Close()
	var version int
	require.NoError(t, raw.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version))
	assert.Equal(t, dataVersion-1, version)
}

func TestMessageIndexConsolidationRetainsAnalyzedPlans(t *testing.T) {
	d := legacyMessageIndexDB(t)
	seedMessageIndexHistory(t, d)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `ANALYZE messages; ANALYZE sessions`)
		return err
	}))
	before := messageIndexRows(t, d.rawReader(), "sqlite_stat1")
	_, err := d.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir(), ConsolidateMessageIndex: true})
	require.NoError(t, err)
	after := messageIndexRows(t, d.rawReader(), "sqlite_stat1")
	var expected [][]any
	for _, row := range before {
		if row[1] != "sqlite_autoindex_messages_1" {
			expected = append(expected, row)
		}
	}
	assert.ElementsMatch(t, expected, after, "surviving message indexes and unrelated session statistics stay populated")
	var removed int
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT count(*) FROM sqlite_stat1 WHERE idx='sqlite_autoindex_messages_1'`).Scan(&removed))
	assert.Zero(t, removed)
	rows, err := d.Reader().QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT id,content FROM messages WHERE session_id='saved-session' AND ordinal>=0 ORDER BY ordinal LIMIT 2`)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, strings.Join(details, "\n"), "SEARCH messages USING INDEX idx_messages_session_ordinal")
	assert.NotContains(t, strings.Join(details, "\n"), "TEMP B-TREE")
}
