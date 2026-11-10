package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compare all 16 logical values and their driver storage types. Digests are
// normalized at the read boundary; every other field is scanned without casts.
func storageFormatsSnapshot(t *testing.T, d *DB) [][]any {
	t.Helper()
	rows, err := d.Reader().Query(t.Context(), `SELECT session_id,message_id,ordinal,role,timestamp,source_id,body,`+
		conversationDigestReadSQL("digest", "archive_metadata")+`,text_bytes,`+
		conversationDigestReadSQL("visible_digest", "archive_metadata")+`,visible_text_bytes,visible_gap,gap,deleted,removed,revision
        FROM conversation_messages ORDER BY session_id,message_id`)
	require.NoError(t, err)
	defer rows.Close()
	var snapshot [][]any
	for rows.Next() {
		values, pointers := make([]any, 16), make([]any, 16)
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(t, rows.Scan(pointers...))
		snapshot = append(snapshot, values)
	}
	require.NoError(t, rows.Err())
	return snapshot
}

func storageFormatsRequirePhysical(t *testing.T, d *DB, refs, digests, archiveOnly bool) {
	t.Helper()
	var version int
	require.NoError(t, d.Reader().QueryRow(t.Context(), "PRAGMA user_version").Scan(&version))
	want := dataVersion
	if refs {
		want |= archiveFormatSessionRefs
	}
	if digests {
		want |= archiveFormatDigestPacking
	}
	if archiveOnly {
		want |= archiveOnlyVersionBase
	}
	assert.Equal(t, want, version, "changing one format must preserve every other capability")
	var kind string
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT type FROM sqlite_master WHERE name='conversation_messages'`).Scan(&kind))
	table := "conversation_messages"
	if refs {
		assert.Equal(t, "view", kind)
		table = "conversation_message_refs"
		var invalid int
		require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT COUNT(*) FROM conversation_message_refs m LEFT JOIN conversation_session_refs r ON r.session_ref=m.session_ref WHERE r.session_ref IS NULL OR typeof(m.session_ref)!='integer'`).Scan(&invalid))
		assert.Zero(t, invalid)
	} else {
		assert.Equal(t, "table", kind)
	}
	var total, blobs int
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT COUNT(*),COALESCE(SUM(typeof(digest)='blob' OR typeof(visible_digest)='blob'),0) FROM `+table).Scan(&total, &blobs))
	if digests && total > 0 {
		assert.Positive(t, blobs, "fixture has canonical digests that must physically pack")
	}
	if !digests {
		assert.Zero(t, blobs, "reversal must not leave raw packed values in a plain archive")
	}
}

func storageFormatsSet(t *testing.T, d *DB, format string, enabled bool) {
	t.Helper()
	if format == "refs" {
		_, err := d.SetConversationSessionRefs(t.Context(), enabled)
		require.NoError(t, err)
	} else {
		_, err := d.SetConversationDigestPacking(t.Context(), enabled)
		require.NoError(t, err)
	}
}

func storageFormatsSeed(t *testing.T, d *DB, history bool) []Message {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{{SessionID: "chat", Ordinal: 0, Role: "assistant", SourceUUID: "first", Content: "hello", DialogueText: new("abc")}}
	require.NoError(t, d.InsertMessages(ctx, msgs))
	_, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	if history {
		require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,timestamp,source_id,body,digest,text_bytes,visible_digest,visible_text_bytes,visible_gap,gap,deleted,removed) VALUES
                ('Case','Opaque',4,'assistant','legacy-time','native',NULL,?,5,?,3,'','',1,1),
                ('case','opaque',5,'user','','','',?,0,'',0,'visible_text_unavailable','identity_ambiguous',1,1),
                ('','',6,'user','','',NULL,'',0,'',0,'','',1,1),
                (?, ?,7,'assistant',?,?,?, ?,17,?,19,'legacy-visible-gap','legacy-gap',1,1)`,
				helloDigest, abcDigest, "UPPERCASE-LEGACY", "NUL\x00session", "NUL\x00message", "time\x00value", "source\x00identity", "body\x00retained", "digest\x00legacy", "visible\x00legacy")
			return err
		}))
	}
	return msgs
}

func storageFormatsAssertPinned(t *testing.T, d *DB, stored, visible ConversationExportResult) {
	t.Helper()
	for _, tc := range []struct {
		mode, text string
		page       ConversationExportResult
	}{{"", "hello", stored}, {"visible", "abc", visible}} {
		var change ConversationChange
		for _, candidate := range tc.page.Changes {
			if candidate.SessionID == "chat" && candidate.Type == "message" {
				change = candidate
				break
			}
		}
		require.NotEmpty(t, change.MessageID)
		got, err := d.GetConversationMessage(t.Context(), ConversationMessageOptions{TextMode: tc.mode, DatabaseID: tc.page.DatabaseID, SessionID: change.SessionID, MessageID: change.MessageID, Revision: change.Revision})
		require.NoError(t, err)
		require.NotNil(t, got.Text)
		assert.Equal(t, tc.text, *got.Text)
	}
}

func TestStorageFormatsBothOrdersPreserveHistoryAndPinnedReads(t *testing.T) {
	for _, enableFirst := range []string{"refs", "digests"} {
		for _, reverseFirst := range []string{"refs", "digests"} {
			t.Run(enableFirst+"_reverse_"+reverseFirst, func(t *testing.T) {
				ctx := t.Context()
				d := testDB(t)
				storageFormatsSeed(t, d, true)
				require.NoError(t, d.EnableArchiveOnly(ctx))
				before, state := storageFormatsSnapshot(t, d), sessionRefArchiveState(ctx, t, d)
				stored, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				visible, err := d.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: "visible"})
				require.NoError(t, err)
				second := "refs"
				if enableFirst == "refs" {
					second = "digests"
				}
				for _, format := range []string{enableFirst, second} {
					storageFormatsSet(t, d, format, true)
					assert.Equal(t, before, storageFormatsSnapshot(t, d))
					assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
					storageFormatsAssertPinned(t, d, stored, visible)
				}
				storageFormatsRequirePhysical(t, d, true, true, true)
				for _, mode := range []string{"", "visible"} {
					expected := stored
					if mode == "visible" {
						expected = visible
					}
					got, err := d.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: mode})
					require.NoError(t, err)
					assert.Equal(t, expected, got, "conversion preserves complete result and checkpoint")
				}
				path := d.Path()
				require.NoError(t, d.Close())
				d, err = OpenIsolated(ctx, path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, d.Close()) })
				assert.Equal(t, before, storageFormatsSnapshot(t, d))
				assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
				_, err = d.Compact(ctx, CompactOptions{StagingDir: t.TempDir()})
				require.NoError(t, err)
				assert.Equal(t, before, storageFormatsSnapshot(t, d))
				assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
				storageFormatsAssertPinned(t, d, stored, visible)
				second = "refs"
				if reverseFirst == "refs" {
					second = "digests"
				}
				refs, digests := true, true
				for _, format := range []string{reverseFirst, second} {
					storageFormatsSet(t, d, format, false)
					if format == "refs" {
						refs = false
					} else {
						digests = false
					}
					storageFormatsRequirePhysical(t, d, refs, digests, true)
					assert.Equal(t, before, storageFormatsSnapshot(t, d))
					assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
					storageFormatsAssertPinned(t, d, stored, visible)
				}
			})
		}
	}
}

func TestStorageFormatsWritesPublishOnlyLogicalChanges(t *testing.T) {
	for _, first := range []string{"refs", "digests"} {
		t.Run(first, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			msgs := storageFormatsSeed(t, d, false)
			storageFormatsSet(t, d, first, true)
			other := "refs"
			if first == "refs" {
				other = "digests"
			}
			storageFormatsSet(t, d, other, true)
			before, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, before.Changes, 1)
			firstID := before.Changes[0].MessageID
			revision := digestRevision(t, d)
			// Append a later row, then ingest an earlier ordinal after it.
			future := Message{SessionID: "chat", Ordinal: 3, Role: "assistant", SourceUUID: "future", Content: "Later", DialogueText: new("Later visible")}
			late := Message{SessionID: "chat", Ordinal: 1, Role: "user", SourceUUID: "late", Content: "Late arrival", DialogueText: new("Late arrival")}
			require.NoError(t, d.InsertMessages(ctx, []Message{future}))
			require.NoError(t, d.InsertMessages(ctx, []Message{late}))
			assert.Equal(t, revision+2, digestRevision(t, d))
			msgs = append(msgs, late, future)
			unchanged, err := d.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: "visible"})
			require.NoError(t, err)
			require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
			quiet, err := d.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: "visible", Checkpoint: unchanged.Checkpoint})
			require.NoError(t, err)
			assert.Empty(t, quiet.Changes)
			assert.Equal(t, unchanged.Checkpoint, quiet.Checkpoint)
			msgs[0].DialogueText = new("Edited visible text")
			require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
			assert.Equal(t, revision+3, digestRevision(t, d))
			changed, err := d.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: "visible", Checkpoint: unchanged.Checkpoint})
			require.NoError(t, err)
			require.Len(t, changed.Changes, 1)
			assert.Equal(t, firstID, changed.Changes[0].MessageID)
			body, err := d.GetConversationMessage(ctx, ConversationMessageOptions{TextMode: "visible", DatabaseID: changed.DatabaseID, SessionID: "chat", MessageID: firstID, Revision: changed.Changes[0].Revision})
			require.NoError(t, err)
			require.NotNil(t, body.Text)
			assert.Equal(t, "Edited visible text", *body.Text)
			_, err = d.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: before.DatabaseID, SessionID: "chat", MessageID: firstID, Revision: before.Changes[0].Revision})
			require.ErrorIs(t, err, ErrConversationRevisionChanged)
			storageFormatsRequirePhysical(t, d, true, true, false)
			require.NoError(t, d.SoftDeleteSession(ctx, "chat"))
			_, err = d.RestoreSession(ctx, "chat")
			require.NoError(t, err)
			require.NoError(t, d.DeleteSession(ctx, "chat"))
			var retained, mapping int
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT COUNT(*) FROM conversation_messages WHERE session_id='chat' AND removed=1 AND deleted=1`).Scan(&retained))
			assert.Equal(t, 3, retained)
			require.NoError(t, d.Reader().QueryRow(ctx, `SELECT COUNT(*) FROM conversation_session_refs WHERE session_id='chat'`).Scan(&mapping))
			assert.Equal(t, 1, mapping, "hard deletion must retain the permanent external ID mapping")
			storageFormatsRequirePhysical(t, d, true, true, false)
		})
	}
}

func TestStorageFormatsInterruptedConversionWithOtherFormatActive(t *testing.T) {
	for _, format := range []string{"refs", "digests"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_reverse_%t", format, reverse), func(t *testing.T) {
				d := testDB(t)
				ctx := t.Context()
				require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
				msgs := make([]Message, 600)
				for i := range msgs {
					msgs[i] = Message{SessionID: "chat", Ordinal: i, Role: "assistant", SourceUUID: fmt.Sprint(i), Content: "hello", DialogueText: new("abc")}
				}
				require.NoError(t, d.InsertMessages(ctx, msgs))
				_, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				other := "refs"
				if format == "refs" {
					other = "digests"
				}
				storageFormatsSet(t, d, other, true)
				if reverse {
					storageFormatsSet(t, d, format, true)
				}
				before, state := storageFormatsSnapshot(t, d), sessionRefArchiveState(ctx, t, d)
				cancelCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				conn, err := d.getWriter().Conn(ctx)
				require.NoError(t, err)
				require.NoError(t, conn.Raw(func(raw any) error {
					raw.(*sqlite3.SQLiteConn).RegisterUpdateHook(func(op int, database, table string, rowid int64) {
						if format == "refs" && op == sqlite3.SQLITE_INSERT && table == "_conversation_messages_stage" && rowid == 513 {
							cancel()
						}
						if format == "digests" && op == sqlite3.SQLITE_UPDATE && table == "conversation_message_refs" && rowid == 257 {
							cancel()
						}
					})
					return nil
				}))
				require.NoError(t, conn.Close())
				if format == "refs" {
					_, err = d.SetConversationSessionRefs(cancelCtx, !reverse)
				} else {
					_, err = d.SetConversationDigestPacking(cancelCtx, !reverse)
				}
				require.ErrorIs(t, err, context.Canceled)
				assert.Equal(t, before, storageFormatsSnapshot(t, d))
				assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
				if format == "digests" {
					var packed int
					require.NoError(t, d.Reader().QueryRow(ctx, `SELECT COUNT(*) FROM conversation_message_refs WHERE typeof(digest)='blob'`).Scan(&packed))
					want := 256
					if reverse {
						want = 344
					}
					assert.Equal(t, want, packed, "one committed batch survives, interrupted batch rolls back")
					storageFormatsRequirePhysical(t, d, true, true, false)
				} else {
					storageFormatsRequirePhysical(t, d, reverse, true, false)
				}
				path := d.Path()
				require.NoError(t, d.Close())
				d, err = OpenIsolated(ctx, path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, d.Close()) })
				assert.Equal(t, before, storageFormatsSnapshot(t, d))
				assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
				storageFormatsSet(t, d, format, !reverse)
				refs, digests := true, true
				if reverse && format == "refs" {
					refs = false
				}
				if reverse && format == "digests" {
					digests = false
				}
				storageFormatsRequirePhysical(t, d, refs, digests, false)
				assert.Equal(t, before, storageFormatsSnapshot(t, d))
				assert.Equal(t, state, sessionRefArchiveState(ctx, t, d))
			})
		}
	}
}

func TestStorageFormatsRebuildInheritanceAndMixedCopies(t *testing.T) {
	for sourceMode := 0; sourceMode < 4; sourceMode++ {
		for destinationMode := 0; destinationMode < 4; destinationMode++ {
			t.Run(fmt.Sprintf("source_%d_destination_%d", sourceMode, destinationMode), func(t *testing.T) {
				ctx := t.Context()
				source := testDB(t)
				storageFormatsSeed(t, source, false)
				require.NoError(t, source.UpsertSession(ctx, Session{ID: "gone", Project: "sample", Machine: "local", Agent: "claude"}))
				require.NoError(t, source.InsertMessages(ctx, []Message{{SessionID: "gone", Role: "assistant", Content: "hello", DialogueText: new("abc"), SourceUUID: "gone-source"}}))
				require.NoError(t, source.DeleteSession(ctx, "gone"))
				storageFormatsSet(t, source, "refs", sourceMode&1 != 0)
				storageFormatsSet(t, source, "digests", sourceMode&2 != 0)
				before := storageFormatsSnapshot(t, source)
				sourcePage, err := source.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				destination, err := OpenIsolated(ctx, filepath.Join(t.TempDir(), "rebuilt.db"))
				require.NoError(t, err)
				defer destination.Close()
				require.NoError(t, destination.CopyArchiveIdentityFrom(ctx, source.Path()))
				storageFormatsRequirePhysical(t, destination, sourceMode&1 != 0, sourceMode&2 != 0, false)
				storageFormatsSet(t, destination, "refs", destinationMode&1 != 0)
				storageFormatsSet(t, destination, "digests", destinationMode&2 != 0)
				_, err = destination.CopyOrphanedDataFrom(ctx, source.Path())
				require.NoError(t, err)
				require.NoError(t, destination.CopySessionMetadataFrom(ctx, source.Path()))
				after := storageFormatsSnapshot(t, destination)
				// Rebuild generations deliberately allocate fresh revisions;
				// every other logical field and retained identity stays exact.
				require.Len(t, after, len(before))
				for i := range before {
					assert.Equal(t, before[i][:15], after[i][:15])
				}
				storageFormatsRequirePhysical(t, destination, destinationMode&1 != 0, destinationMode&2 != 0, false)
				page, err := destination.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				assert.Equal(t, sourcePage.ArchiveID, page.ArchiveID)
				assert.NotEqual(t, sourcePage.DatabaseID, page.DatabaseID)
				_, err = destination.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: sourcePage.Checkpoint})
				require.ErrorIs(t, err, ErrConversationReconciliationRequired)
				visible, err := destination.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: "visible"})
				require.NoError(t, err)
				storageFormatsAssertPinned(t, destination, page, visible)
			})
		}
	}
}
