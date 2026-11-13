package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

func sessionRefSnapshot(ctx context.Context, tb testing.TB, d *DB) [][]any {
	tb.Helper()
	rows, err := d.Reader().Query(ctx, `SELECT `+conversationPhysicalColumns+` FROM conversation_messages ORDER BY session_id,message_id`)
	require.NoError(tb, err)
	defer rows.Close()
	var all [][]any
	for rows.Next() {
		values, pointers := make([]any, 16), make([]any, 16)
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(tb, rows.Scan(pointers...))
		all = append(all, values)
	}
	require.NoError(tb, rows.Err())
	return all
}

func sessionRefArchiveState(ctx context.Context, tb testing.TB, d *DB) []string {
	tb.Helper()
	var archive, database, revision string
	require.NoError(tb, d.Reader().QueryRow(ctx, `SELECT (SELECT value FROM archive_metadata WHERE key='archive_id'),(SELECT value FROM archive_metadata WHERE key='database_id'),COALESCE((SELECT value FROM archive_metadata WHERE key='conversation_publication_revision'),'0')`).Scan(&archive, &database, &revision))
	return []string{archive, database, revision}
}

func TestConversationSessionRefsExactReversal(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	_, err := d.EnsureConversationExportInitialized(ctx)
	require.NoError(t, err)
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,timestamp,source_id,body,digest,text_bytes,visible_digest,visible_text_bytes,visible_gap,gap,deleted,removed,revision) VALUES
 ('Case','A',0,'assistant','legacy-time','native',NULL,'legacy-digest',57,'visible-legacy',13,'visible-gap','identity_ambiguous',0,0,7),
 ('Case','a',1,'user','','','','',0,'',0,'visible_text_unavailable','',1,1,11),
 ('case','A',2,'assistant','',CAST(x'73006f' AS TEXT),'','digest',5,'v',3,'','',0,0,19),
 ('','',3,'user','','',NULL,'',0,'',0,'visible_text_unavailable','',1,1,23),
 (CAST(x'4300617365' AS TEXT),CAST(x'6d006964' AS TEXT),9,'assistant','','opaque',CAST(x'62006f6479' AS TEXT),'legacy-not-sha',6,'v-old',4,'vg','gap',1,0,31);
 INSERT INTO conversation_session_changes VALUES('session-only',89,1,'archive_content_excluded');
 INSERT INTO archive_metadata(key,value) VALUES('conversation_publication_revision','123') ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
		return err
	}))
	before, state := sessionRefSnapshot(ctx, t, d), sessionRefArchiveState(ctx, t, d)
	for _, enabled := range []bool{true, false} {
		report, err := d.SetConversationSessionRefs(ctx, enabled)
		require.NoError(t, err)
		assert.True(t, report.Changed)
		assert.Equal(t, int64(5), report.Rows)
		assert.Equal(t, before, sessionRefSnapshot(ctx, t, d))
		assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
		var version int
		require.NoError(t, d.Reader().QueryRow(ctx, "PRAGMA user_version").Scan(&version))
		assert.Equal(t, enabled, version&archiveFormatSessionRefs != 0)
		if enabled {
			_, err = d.Compact(ctx, CompactOptions{})
			require.NoError(t, err)
			assert.Equal(t, before, sessionRefSnapshot(ctx, t, d))
			assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
		}
		ro, err := OpenReadOnly(ctx, d.Path())
		require.NoError(t, err)
		assert.Equal(t, before, sessionRefSnapshot(ctx, t, ro))
		require.NoError(t, ro.Close())
		path := d.Path()
		require.NoError(t, d.Close())
		d, err = OpenIsolated(ctx, path)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, d.Close()) })
		assert.Equal(t, before, sessionRefSnapshot(ctx, t, d))
		assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
	}
	var nulSession, nulMessage, body string
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT session_id,message_id,body FROM conversation_messages WHERE ordinal=9`).Scan(&nulSession, &nulMessage, &body))
	assert.Equal(t, "C\x00ase", nulSession)
	assert.Equal(t, "m\x00id", nulMessage)
	assert.Equal(t, "b\x00ody", body)
	raw, err := sql.Open("sqlite3", makeDSN(d.Path(), false))
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.ExecContext(ctx, `UPDATE conversation_messages SET digest='ordinary write' WHERE session_id='Case' AND message_id='A'`)
	require.NoError(t, err, "reversal restores ordinary SQLite writes")
}

func TestConversationSessionRefsWritesAndCursors(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(fmt.Sprintf("cold=%t", cold), func(t *testing.T) {
			ctx := t.Context()
			d := testDB(t)
			insertSession(t, d, "chat", "sample")
			msgs := []Message{{SessionID: "chat", Ordinal: 0, Role: "user", SourceUUID: "one", Content: "Question", DialogueText: new("Question")}, {SessionID: "chat", Ordinal: 1, Role: "assistant", SourceUUID: "two", Content: "Reply", DialogueText: new("Reply")}}
			require.NoError(t, d.InsertMessages(ctx, msgs))
			var initial ConversationExportResult
			var err error
			if !cold {
				initial, err = d.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
			}
			_, err = d.SetConversationSessionRefs(ctx, true)
			require.NoError(t, err)
			if cold {
				var active bool
				require.NoError(t, d.Reader().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_metadata WHERE key='conversation_export_initialized')`).Scan(&active))
				assert.False(t, active)
				initial, err = d.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
			}
			quiet, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
			require.NoError(t, err)
			assert.Empty(t, quiet.Changes)
			assert.Equal(t, initial.Checkpoint, quiet.Checkpoint)
			first := initial.Changes[0]
			body, err := d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: initial.DatabaseID, SessionID: "chat", MessageID: first.MessageID, Revision: first.Revision})
			require.NoError(t, err)
			require.NotNil(t, body.Text)
			assert.Equal(t, "Question", *body.Text)
			msgs[1].Content = "Revised"
			msgs[1].DialogueText = new("Revised")
			require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
			changed, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
			require.NoError(t, err)
			require.Len(t, changed.Changes, 1)
			assert.Equal(t, initial.Changes[1].MessageID, changed.Changes[0].MessageID)
			require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
			quiet, err = d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: changed.Checkpoint})
			require.NoError(t, err)
			assert.Empty(t, quiet.Changes)
			visible, err := d.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: "visible"})
			require.NoError(t, err)
			msgs[1].DialogueText = new("Short")
			require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
			visibleDelta, err := d.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: "visible", Checkpoint: visible.Checkpoint})
			require.NoError(t, err)
			require.Len(t, visibleDelta.Changes, 1)
			visibleBody, err := d.GetConversationMessage(ctx, ConversationMessageOptions{TextMode: "visible", DatabaseID: visible.DatabaseID, SessionID: "chat", MessageID: visibleDelta.Changes[0].MessageID, Revision: visibleDelta.Changes[0].Revision})
			require.NoError(t, err)
			require.NotNil(t, visibleBody.Text)
			assert.Equal(t, "Short", *visibleBody.Text)
			// Missing native output becomes a retained tombstone, then returns with its opaque ID.
			require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs[:1]))
			require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
			returned, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			assert.Equal(t, initial.Changes[1].MessageID, returned.Changes[1].MessageID)
			require.NoError(t, d.SoftDeleteSession(ctx, "chat"))
			_, err = d.RestoreSession(ctx, "chat")
			require.NoError(t, err)
			require.NoError(t, d.DeleteSession(ctx, "chat"))
			deleted, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, deleted.Changes, 3)
			for _, change := range deleted.Changes {
				assert.True(t, change.Deleted)
			}
			var refs int
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT COUNT(*) FROM conversation_session_refs WHERE session_id='chat'`).Scan(&refs))
			assert.Equal(t, 1, refs)
		})
	}
}

func TestConversationSessionRefsInterruptedCopy(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			ctx := t.Context()
			d := testDB(t)
			insertSession(t, d, "chat", "sample")
			msgs := make([]Message, 600)
			for i := range msgs {
				msgs[i] = Message{SessionID: "chat", Ordinal: i, Role: "assistant", SourceUUID: fmt.Sprint(i), Content: "Retained reply"}
			}
			require.NoError(t, d.InsertMessages(ctx, msgs))
			_, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			if reverse {
				_, err = d.SetConversationSessionRefs(ctx, true)
				require.NoError(t, err)
			}
			before, state := sessionRefSnapshot(ctx, t, d), sessionRefArchiveState(ctx, t, d)
			physical := "conversation_messages"
			if reverse {
				physical = "conversation_message_refs"
			}
			assert.Contains(t, queryPlanOf(t, d, conversationRefBatchBoundarySQL(physical, true), 256), "rowid>?")
			cancelCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			conn, err := d.getWriter().Conn(ctx)
			require.NoError(t, err)
			require.NoError(t, conn.Raw(func(raw any) error {
				raw.(*sqlite3.SQLiteConn).RegisterUpdateHook(func(op int, database, table string, rowid int64) {
					if op == sqlite3.SQLITE_INSERT && table == "_conversation_messages_stage" && rowid == 513 {
						cancel()
					}
				})
				return nil
			}))
			require.NoError(t, conn.Close())
			_, err = d.SetConversationSessionRefs(cancelCtx, !reverse)
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, before, sessionRefSnapshot(ctx, t, d))
			assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
			path := d.Path()
			require.NoError(t, d.Close())
			d, err = OpenIsolated(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, d.Close()) })
			_, err = d.SetConversationSessionRefs(ctx, !reverse)
			require.NoError(t, err)
			assert.Equal(t, before, sessionRefSnapshot(ctx, t, d))
			assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
		})
	}
}

func TestConversationSessionRefsRebuildCopies(t *testing.T) {
	for _, sourceRefs := range []bool{false, true} {
		for _, destRefs := range []bool{false, true} {
			t.Run(fmt.Sprintf("source=%t/dest=%t", sourceRefs, destRefs), func(t *testing.T) {
				ctx := t.Context()
				source := testDB(t)
				dest, openErr := OpenIsolated(ctx, filepath.Join(t.TempDir(), "rebuilt.db"))
				require.NoError(t, openErr)
				t.Cleanup(func() { require.NoError(t, dest.Close()) })
				for _, id := range []string{"orphan", "trash", "deleted"} {
					insertSession(t, source, id, "sample")
					insertMessages(t, source, Message{SessionID: id, Role: "assistant", SourceUUID: "native", Content: "Retained text"})
				}
				before, err := source.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				require.NoError(t, source.SoftDeleteSession(ctx, "trash"))
				require.NoError(t, source.DeleteSession(ctx, "deleted"))
				if sourceRefs {
					_, err = source.SetConversationSessionRefs(ctx, true)
					require.NoError(t, err)
				}
				if destRefs {
					_, err = dest.SetConversationSessionRefs(ctx, true)
					require.NoError(t, err)
				}
				require.NoError(t, dest.CopyArchiveIdentityFrom(ctx, source.Path()))
				_, err = dest.CopyOrphanedDataFrom(ctx, source.Path())
				require.NoError(t, err)
				require.NoError(t, dest.CopySessionMetadataFrom(ctx, source.Path()))
				after, err := dest.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				assert.Equal(t, before.ArchiveID, after.ArchiveID)
				assert.NotEqual(t, before.DatabaseID, after.DatabaseID)
				ids := map[string]string{}
				for _, change := range before.Changes {
					ids[change.SessionID] = change.MessageID
				}
				for _, change := range after.Changes {
					if change.Type == "message" {
						assert.Equal(t, ids[change.SessionID], change.MessageID)
						assert.Equal(t, change.SessionID != "orphan", change.Deleted)
					}
				}
				_, err = dest.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: before.Checkpoint})
				require.ErrorIs(t, err, ErrConversationReconciliationRequired)
			})
		}
	}
}

func TestConversationSessionRefsFormatGate(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.EnableArchiveOnly(ctx))
	_, err := d.SetConversationSessionRefs(ctx, true)
	require.NoError(t, err)
	var version int
	require.NoError(t, d.Reader().QueryRow(ctx, "PRAGMA user_version").Scan(&version))
	parser, archiveOnly := DecodeUserVersion(version)
	assert.True(t, archiveOnly)
	assert.Equal(t, dataVersion, parser)
	assert.Greater(t, version-archiveOnlyVersionBase, dataVersion, "old runtimes reject this storage version")
	_, err = d.SetConversationSessionRefs(ctx, false)
	require.NoError(t, err)
	require.NoError(t, d.Reader().QueryRow(ctx, "PRAGMA user_version").Scan(&version))
	assert.Equal(t, archiveOnlyVersionBase+dataVersion, version)
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES('conversation_session_refs','future')`)
		return err
	}))
	_, err = OpenReadOnly(ctx, d.Path())
	require.ErrorContains(t, err, "unsupported archive storage format")
}

func sessionRefBenchmarkFixture(b *testing.B) (*DB, []Message, ConversationExportResult) {
	b.Helper()
	d := testDB(b)
	ctx := b.Context()
	sessionID := strings.Repeat("opaque-session-", 4)
	require.NoError(b, d.UpsertSession(ctx, Session{ID: sessionID, Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := make([]Message, 10000)
	for i := range msgs {
		msgs[i] = Message{SessionID: sessionID, Ordinal: i, Role: "assistant", SourceUUID: fmt.Sprint(i), Content: "Retained text"}
	}
	require.NoError(b, d.InsertMessages(ctx, msgs))
	initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(b, err)
	return d, msgs, initial
}

func BenchmarkConversationSessionReferences(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		for _, operation := range []string{"read", "refresh"} {
			b.Run(fmt.Sprintf("refs=%t/%s", enabled, operation), func(b *testing.B) {
				d, msgs, initial := sessionRefBenchmarkFixture(b)
				ctx := b.Context()
				if enabled {
					_, err := d.SetConversationSessionRefs(ctx, true)
					require.NoError(b, err)
				}
				_, err := d.getWriter().ExecContext(ctx, "VACUUM; ANALYZE")
				require.NoError(b, err)
				var pages, size int64
				require.NoError(b, d.Reader().QueryRow(ctx, "PRAGMA page_count").Scan(&pages))
				require.NoError(b, d.Reader().QueryRow(ctx, "PRAGMA page_size").Scan(&size))
				b.ReportAllocs()
				i := 0
				for b.Loop() {
					if operation == "read" {
						_, err = d.ExportConversationChanges(ctx, ConversationExportOptions{Cursor: initial.NextCursor, Limit: 100})
					} else {
						// Refresh the full 10,000-message session, including source-ID
						// matching, session-ref lookup, and publication triggers.
						msgs[0].Content = []string{"Changed text A", "Changed text B"}[i%2]
						err = d.ReplaceSessionMessages(ctx, msgs[0].SessionID, msgs)
						i++
					}
					require.NoError(b, err)
				}
				b.ReportMetric(float64(pages*size), "fixture-db-bytes")
			})
		}
	}
}

func BenchmarkConversationSessionReferenceConversion(b *testing.B) {
	for _, enable := range []bool{true, false} {
		b.Run(fmt.Sprintf("enable=%t", enable), func(b *testing.B) {
			d, _, _ := sessionRefBenchmarkFixture(b)
			ctx := b.Context()
			if !enable {
				_, err := d.SetConversationSessionRefs(ctx, true)
				require.NoError(b, err)
			}
			before := sessionRefSnapshot(ctx, b, d)
			state := sessionRefArchiveState(ctx, b, d)
			b.ReportAllocs()
			for b.Loop() {
				report, err := d.SetConversationSessionRefs(ctx, enable)
				require.NoError(b, err)
				require.Equal(b, int64(10000), report.Rows)
				b.StopTimer()
				_, err = d.SetConversationSessionRefs(ctx, !enable)
				require.NoError(b, err)
				b.StartTimer()
			}
			require.Equal(b, before, sessionRefSnapshot(ctx, b, d))
			require.Equal(b, state, sessionRefArchiveState(ctx, b, d))
			b.ReportMetric(10000, "rows/op")
		})
	}
}

func TestConversationSessionRefsUsagePolicyCopy(t *testing.T) {
	ctx := t.Context()
	source, dest := testDB(t), testDB(t)
	insertSession(t, source, "retained", "sample")
	insertMessages(t, source, Message{SessionID: "retained", Role: "assistant", SourceUUID: "native", Content: "Archived prose", DialogueText: new("Archived prose")})
	before, err := source.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	_, err = source.SetConversationSessionRefs(ctx, true)
	require.NoError(t, err)
	dest.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, dest.CopyArchiveIdentityFrom(ctx, source.Path()))
	_, err = dest.CopyOrphanedDataFrom(ctx, source.Path())
	require.NoError(t, err)
	after, err := dest.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: "visible"})
	require.NoError(t, err)
	var message ConversationChange
	for _, change := range after.Changes {
		if change.Type == "message" {
			message = change
		}
	}
	assert.Equal(t, before.Changes[0].MessageID, message.MessageID)
	assert.Equal(t, "archive_content_excluded", message.Gap)
	assert.Empty(t, message.Digest)
	assert.Zero(t, message.TextBytes)
}

func TestConversationSessionRefsRejectInvalidAttachedFormats(t *testing.T) {
	for _, tc := range []struct{ name, sql, want string }{
		{"missing-bit", fmt.Sprintf("PRAGMA user_version=%d", dataVersion), "disagrees"},
		{"missing-marker", "DELETE FROM archive_metadata WHERE key='conversation_session_refs'", "disagrees"},
		{"empty-mode", "UPDATE archive_metadata SET value='' WHERE key='conversation_session_refs'", "unsupported archive storage format"},
		{"unknown-mode", "UPDATE archive_metadata SET value='future' WHERE key='conversation_session_refs'", "unsupported archive storage format"},
		{"unknown-bit", fmt.Sprintf("PRAGMA user_version=%d", dataVersion|archiveFormatSessionRefs|1<<26), "unsupported"},
		{"negative-version", "PRAGMA user_version=-2147483648", "negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			source, dest := testDB(t), testDB(t)
			insertSession(t, source, "retained", "sample")
			insertMessages(t, source, Message{SessionID: "retained", Role: "assistant", Content: "Original text"})
			_, err := source.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			_, err = source.SetConversationSessionRefs(ctx, true)
			require.NoError(t, err)
			require.NoError(t, dest.CopyArchiveIdentityFrom(ctx, source.Path()))
			require.NoError(t, source.Update(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, tc.sql)
				return err
			}))
			before, state := sessionRefSnapshot(ctx, t, dest), sessionRefArchiveState(ctx, t, dest)
			require.ErrorContains(t, dest.CopyArchiveIdentityFrom(ctx, source.Path()), tc.want)
			_, err = dest.CopyOrphanedDataFrom(ctx, source.Path())
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, dest.CopySessionMetadataFrom(ctx, source.Path()), tc.want)
			assert.Equal(t, before, sessionRefSnapshot(ctx, t, dest))
			assert.Equal(t, state, sessionRefArchiveState(ctx, t, dest))
			_, err = OpenReadOnly(ctx, source.Path())
			require.Error(t, err)
		})
	}
}

func TestConversationSessionRefsRejectsSchemaExtensions(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, kind := range []string{"index", "trigger", "column"} {
			t.Run(fmt.Sprintf("reverse=%t/%s", reverse, kind), func(t *testing.T) {
				ctx := t.Context()
				d := testDB(t)
				insertSession(t, d, "chat", "sample")
				insertMessages(t, d, Message{SessionID: "chat", Role: "assistant", Content: "Preserved text"})
				_, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				table := "conversation_messages"
				if reverse {
					_, err = d.SetConversationSessionRefs(ctx, true)
					require.NoError(t, err)
					table = "conversation_message_refs"
				}
				statement := "CREATE INDEX custom_conversation_index ON " + table + "(body)"
				want := "unrecognized schema object"
				switch kind {
				case "trigger":
					statement = "CREATE TRIGGER custom_conversation_trigger AFTER UPDATE ON " + table + " BEGIN SELECT 1; END"
				case "column":
					statement = "ALTER TABLE " + table + " ADD COLUMN extension TEXT NOT NULL DEFAULT 'retained'"
					want = "16-column layout"
				}
				require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, statement); return err }))
				before, state := sessionRefSnapshot(ctx, t, d), sessionRefArchiveState(ctx, t, d)
				_, err = d.SetConversationSessionRefs(ctx, !reverse)
				require.ErrorContains(t, err, want)
				assert.Equal(t, before, sessionRefSnapshot(ctx, t, d))
				assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
				var retained bool
				if kind == "column" {
					require.NoError(t, d.Reader().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name='extension')`, table).Scan(&retained))
				} else {
					require.NoError(t, d.Reader().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name=?)`, "custom_conversation_"+kind).Scan(&retained))
				}
				assert.True(t, retained)
			})
		}
	}
}

func TestConversationSessionRefsRefusesMissingMapping(t *testing.T) {
	ctx := t.Context()
	d := testDB(t)
	insertSession(t, d, "retained", "sample")
	insertMessages(t, d, Message{SessionID: "retained", Role: "assistant", Content: "Retained text"})
	_, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	_, err = d.SetConversationSessionRefs(ctx, true)
	require.NoError(t, err)
	// Model an external importer that bypasses foreign-key enforcement.
	_, err = d.getWriter().ExecContext(ctx, `PRAGMA foreign_keys=OFF; DELETE FROM conversation_session_refs; PRAGMA foreign_keys=ON`)
	require.NoError(t, err)
	var physical, logical int
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT COUNT(*) FROM conversation_message_refs`).Scan(&physical))
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT COUNT(*) FROM conversation_messages`).Scan(&logical))
	require.Equal(t, 1, physical)
	require.Zero(t, logical, "the logical view cannot see the orphaned physical history")
	_, err = d.SetConversationSessionRefs(ctx, false)
	require.ErrorContains(t, err, "missing their session mapping")
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT COUNT(*) FROM conversation_message_refs`).Scan(&physical))
	assert.Equal(t, 1, physical, "refusal must retain the unreadable physical row for recovery")
	var mode string
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT value FROM archive_metadata WHERE key='conversation_session_refs'`).Scan(&mode))
	assert.Equal(t, "v1", mode)
	dest := testDB(t)
	before, state := sessionRefSnapshot(ctx, t, dest), sessionRefArchiveState(ctx, t, dest)
	require.ErrorContains(t, dest.CopyArchiveIdentityFrom(ctx, d.Path()), "missing their session mapping")
	_, err = dest.CopyOrphanedDataFrom(ctx, d.Path())
	require.ErrorContains(t, err, "missing their session mapping")
	_, err = dest.CopyTrashedDataFrom(ctx, d.Path())
	require.ErrorContains(t, err, "missing their session mapping")
	require.ErrorContains(t, dest.CopySessionMetadataFrom(ctx, d.Path()), "missing their session mapping")
	assert.Equal(t, before, sessionRefSnapshot(ctx, t, dest))
	assert.Equal(t, state, sessionRefArchiveState(ctx, t, dest))
}

// Exercise our classification boundary directly so both race outcomes remain
// covered even when database/sql wins the scheduling race only intermittently
// in TestConversationSessionRefsInterruptedCopy.
func TestConversationSessionRefsConversionError(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer stop()
	wrapped := fmt.Errorf("commit copied batch: %w", sql.ErrTxDone)
	storageFailure := errors.New("fixture storage failure")
	for _, tc := range []struct {
		name        string
		deadline    bool
		input       error
		wantContext error
	}{
		{"canceled rollback", false, sql.ErrTxDone, context.Canceled},
		{"wrapped rollback", false, wrapped, context.Canceled},
		{"deadline rollback", true, sql.ErrTxDone, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := canceled
			if tc.deadline {
				ctx = expired
			}
			err := conversationRefConversionError(ctx, tc.input)
			require.ErrorIs(t, err, tc.wantContext)
			assert.ErrorIs(t, err, sql.ErrTxDone)
			assert.ErrorIs(t, err, tc.input, "retain the underlying error and diagnostic wrapper")
		})
	}
	assert.NoError(t, conversationRefConversionError(canceled, nil), "cancellation after a successful cutover must not report failure")
	assert.Same(t, wrapped, conversationRefConversionError(t.Context(), wrapped), "an unrelated finished transaction is not cancellation")
	assert.Same(t, storageFailure, conversationRefConversionError(canceled, storageFailure), "do not reclassify other storage failures")
}
