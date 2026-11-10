package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helloDigest = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
const abcDigest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

func digestRevision(t *testing.T, d *DB) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key='conversation_publication_revision'),0)`).Scan(&revision))
	return revision
}

func TestDigestPackingExportsAndReconciliation(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := []Message{{SessionID: "chat", Role: "assistant", Content: "hello", DialogueText: new("abc"), SourceUUID: "source"}}
	require.NoError(t, d.InsertMessages(ctx, msgs))
	stored, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	visible, err := d.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: "visible"})
	require.NoError(t, err)
	require.Len(t, stored.Changes, 1)
	assert.Equal(t, helloDigest, stored.Changes[0].Digest)
	assert.Equal(t, abcDigest, visible.Changes[0].Digest)
	revision := digestRevision(t, d)
	result, err := d.SetConversationDigestPacking(ctx, true)
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.ChangedRows)
	var kind, visibleKind string
	var size, visibleSize int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT typeof(digest),length(digest),typeof(visible_digest),length(visible_digest) FROM conversation_messages`).Scan(&kind, &size, &visibleKind, &visibleSize))
	assert.Equal(t, "blob", kind)
	assert.Equal(t, "blob", visibleKind)
	assert.Equal(t, 32, size)
	assert.Equal(t, 32, visibleSize)
	for _, mode := range []string{"", "visible"} {
		before := stored
		wantText := "hello"
		if mode == "visible" {
			before = visible
			wantText = "abc"
		}
		got, err := d.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: mode})
		require.NoError(t, err)
		assert.Equal(t, before, got, "physical conversion preserves cursors, identity, revisions and both projections")
		change := before.Changes[0]
		body, err := d.GetConversationMessage(ctx, ConversationMessageOptions{TextMode: mode, DatabaseID: before.DatabaseID, SessionID: change.SessionID, MessageID: change.MessageID, Revision: change.Revision})
		require.NoError(t, err)
		require.NotNil(t, body.Text)
		assert.Equal(t, wantText, *body.Text)
	}
	require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
	assert.Equal(t, revision, digestRevision(t, d), "unchanged reparses must not republish packed digests")
	msgs[0].Content = "changed stored text"
	msgs[0].DialogueText = new("changed visible text")
	require.NoError(t, d.ReplaceSessionMessages(ctx, "chat", msgs))
	assert.Equal(t, revision+1, digestRevision(t, d), "one change to both projections publishes once")
	delta, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: stored.Checkpoint})
	require.NoError(t, err)
	require.Len(t, delta.Changes, 1)
	assert.Equal(t, stored.Changes[0].MessageID, delta.Changes[0].MessageID)
	beforeUnpack, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	_, err = d.SetConversationDigestPacking(ctx, false)
	require.NoError(t, err)
	afterUnpack, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	assert.Equal(t, beforeUnpack, afterUnpack)
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT typeof(digest),typeof(visible_digest) FROM conversation_messages`).Scan(&kind, &visibleKind))
	assert.Equal(t, "text", kind)
	assert.Equal(t, "text", visibleKind)
	var version int
	require.NoError(t, d.getReader().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version))
	assert.Equal(t, dataVersion, version)
	// Plain SQLite can write again after reversal; no custom digest function
	// remains in the restored publication triggers.
	raw, err := sql.Open("sqlite3", d.Path())
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.ExecContext(ctx, `UPDATE conversation_messages SET digest='legacy',visible_digest='legacy visible'`)
	require.NoError(t, err)
}

func TestDigestPackingExactTextFallbackAndFutureWrites(t *testing.T) {
	d := testDB(t)
	inputs := []string{helloDigest, "", strings.ToUpper(helloDigest), "legacy:opaque", "abc\x00tail", strings.Repeat("a", 31) + "\x00" + strings.Repeat("a", 32), strings.Repeat("é", 32), strings.Repeat("0", 63), strings.Repeat("0", 65)}
	for i, value := range inputs {
		_, err := d.getWriter().Exec(t.Context(), `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,digest,visible_digest,deleted,removed) VALUES('legacy',?,?,'assistant',?,?,1,1)`, fmt.Sprint(i), i, value, value)
		require.NoError(t, err)
	}
	revision := digestRevision(t, d)
	for _, enabled := range []bool{true, false, true} {
		_, err := d.SetConversationDigestPacking(t.Context(), enabled)
		require.NoError(t, err)
		assert.Equal(t, revision, digestRevision(t, d))
		for i, want := range inputs {
			var got, kind, visible string
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT agentsview_digest_text(digest),typeof(digest),agentsview_digest_text(visible_digest) FROM conversation_messages WHERE message_id=?`, fmt.Sprint(i)).Scan(&got, &kind, &visible))
			assert.Equal(t, want, got)
			assert.Equal(t, want, visible)
			wantKind := "text"
			if enabled && i == 0 {
				wantKind = "blob"
			}
			assert.Equal(t, wantKind, kind)
		}
	}
	_, err := d.getWriter().Exec(t.Context(), `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,digest,visible_digest) VALUES('new','future',0,'user',?,?)`, helloDigest, abcDigest)
	require.NoError(t, err)
	assert.Equal(t, revision+1, digestRevision(t, d))
	var size int
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT length(digest) FROM conversation_messages WHERE session_id='new'`).Scan(&size))
	assert.Equal(t, 32, size)
}

func TestDigestPackingTriggerOrderAndRecursiveUpdates(t *testing.T) {
	for _, reverseOrder := range []bool{false, true} {
		t.Run(fmt.Sprint(reverseOrder), func(t *testing.T) {
			d := testDB(t)
			_, err := d.SetConversationDigestPacking(t.Context(), true)
			require.NoError(t, err)
			_, err = d.getWriter().Exec(t.Context(), `PRAGMA recursive_triggers=ON`)
			require.NoError(t, err)
			if reverseOrder {
				for _, name := range []string{"conversation_messages_revise", "conversation_visible_revise", "conversation_messages_revision"} {
					var ddl string
					require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&ddl))
					_, err = d.getWriter().Exec(t.Context(), `DROP TRIGGER `+name)
					require.NoError(t, err)
					_, err = d.getWriter().Exec(t.Context(), ddl)
					require.NoError(t, err)
				}
			}
			_, err = d.getWriter().Exec(t.Context(), `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,digest,visible_digest) VALUES('chat','opaque',0,'user',?,?)`, helloDigest, abcDigest)
			require.NoError(t, err)
			revision := digestRevision(t, d)
			assert.EqualValues(t, 1, revision)
			_, err = d.getWriter().Exec(t.Context(), `UPDATE conversation_messages SET digest=?,visible_digest=?`, helloDigest, abcDigest)
			require.NoError(t, err)
			assert.Equal(t, revision, digestRevision(t, d), "identical TEXT replacing packed values is a logical no-op")
			_, err = d.getWriter().Exec(t.Context(), `UPDATE conversation_messages SET digest=?,visible_digest=?`, abcDigest, helloDigest)
			require.NoError(t, err)
			assert.Equal(t, revision+1, digestRevision(t, d), "changed stored and visible digests publish once regardless of trigger creation order")
			_, err = d.SetConversationDigestPacking(t.Context(), false)
			require.NoError(t, err)
			assert.Equal(t, revision+1, digestRevision(t, d))
		})
	}
}

func TestDigestPackingRejectsUnmarkedBlobsAndMalformedValues(t *testing.T) {
	for _, value := range [][]byte{make([]byte, 32), {1}, {}} {
		t.Run(fmt.Sprint(len(value)), func(t *testing.T) {
			d := testDB(t)
			_, err := d.getWriter().Exec(t.Context(), `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,digest) VALUES('chat','opaque',0,'user',?)`, value)
			require.NoError(t, err)
			_, err = d.SetConversationDigestPacking(t.Context(), true)
			require.ErrorContains(t, err, "existing non-TEXT")
			var version int
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version))
			assert.Equal(t, dataVersion, version)
			var original []byte
			require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT digest FROM conversation_messages`).Scan(&original))
			assert.Equal(t, value, original)
			var logical string
			err = d.getReader().QueryRowContext(t.Context(), `SELECT `+conversationDigestReadSQL("digest", "archive_metadata")+` FROM conversation_messages`).Scan(&logical)
			require.ErrorContains(t, err, "non-TEXT conversation digest")
		})
	}
	d := testDB(t)
	var decoded string
	err := d.getReader().QueryRowContext(t.Context(), `SELECT agentsview_digest_text(X'01')`).Scan(&decoded)
	require.ErrorContains(t, err, "require 32")
	_, err = d.SetConversationDigestPacking(t.Context(), true)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,visible_digest) VALUES('chat','opaque',0,'user',X'01')`)
	require.ErrorContains(t, err, "invalid packed conversation digest")
}

func TestDigestPackingInterruptionReopenAndVersionFence(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for i := range 300 {
		_, err := d.getWriter().Exec(ctx, `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,digest,visible_digest,deleted,removed) VALUES('removed',?,?,'assistant',?,?,1,1)`, fmt.Sprint(i), i, helloDigest, abcDigest)
		require.NoError(t, err)
	}
	require.NoError(t, d.EnableArchiveOnly(ctx))
	revision := digestRevision(t, d)
	// Stop at a real commit boundary, rather than inventing an inconsistent
	// schema. Neither reopen nor a read may resume the expensive conversion.
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error { return setConversationDigestModeTx(ctx, tx, true) }))
	last, scanned, changed, err := d.convertConversationDigestBatch(ctx, true, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 256, scanned)
	assert.EqualValues(t, 256, changed)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, _, err = d.convertConversationDigestBatch(canceled, true, &last)
	require.ErrorIs(t, err, context.Canceled)
	path := d.Path()
	require.NoError(t, d.Close())
	reopened, err := OpenIsolated(ctx, path)
	require.NoError(t, err)
	defer reopened.Close()
	var packed, version int
	require.NoError(t, reopened.getReader().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_messages WHERE typeof(digest)='blob'`).Scan(&packed))
	assert.Equal(t, 256, packed, "startup must not finish a maintenance scan")
	require.NoError(t, reopened.getReader().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version))
	assert.Equal(t, dataVersion+archiveOnlyVersionBase+archiveFormatDigestPacking, version)
	assert.Greater(t, version-archiveOnlyVersionBase, dataVersion, "the previous decoder rejects the packed format")
	_, err = reopened.SetConversationDigestPacking(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, revision, digestRevision(t, reopened))
	require.NoError(t, reopened.Update(ctx, func(tx *sql.Tx) error { return setConversationDigestModeTx(ctx, tx, false) }))
	_, _, changed, err = reopened.convertConversationDigestBatch(ctx, false, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 256, changed)
	reader, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	_, err = reopened.SetConversationDigestPacking(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, revision, digestRevision(t, reopened))
	require.NoError(t, reopened.getReader().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version))
	assert.Equal(t, dataVersion+archiveOnlyVersionBase, version)
	require.NoError(t, reopened.getReader().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_messages WHERE removed=1 AND typeof(digest)='text' AND digest=?`, helloDigest).Scan(&packed))
	assert.Equal(t, 300, packed)
}

func TestDigestPackingArchiveCopiesBothDirections(t *testing.T) {
	for _, sourcePacked := range []bool{false, true} {
		for _, destinationPacked := range []bool{false, true} {
			t.Run(fmt.Sprintf("source_%t_destination_%t", sourcePacked, destinationPacked), func(t *testing.T) {
				source := testDB(t)
				ctx := t.Context()
				require.NoError(t, source.UpsertSession(ctx, Session{ID: "orphan", Project: "sample", Machine: "local", Agent: "claude"}))
				require.NoError(t, source.InsertMessages(ctx, []Message{{SessionID: "orphan", Role: "assistant", Content: "hello", DialogueText: new("abc"), SourceUUID: "opaque"}}))
				before, err := source.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				if sourcePacked {
					_, err = source.SetConversationDigestPacking(ctx, true)
					require.NoError(t, err)
				}
				destination, err := OpenIsolated(ctx, filepath.Join(t.TempDir(), "copy.db"))
				require.NoError(t, err)
				defer destination.Close()
				require.NoError(t, destination.CopyArchiveIdentityFrom(ctx, source.Path()))
				// Identity inheritance should retain the source's requested
				// format; an explicit destination choice can override it.
				var mode string
				require.NoError(t, destination.getReader().QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM archive_metadata WHERE key='conversation_digest_packing'),'')`).Scan(&mode))
				if sourcePacked {
					assert.Equal(t, "v1", mode)
				} else {
					assert.Empty(t, mode)
				}
				_, err = destination.SetConversationDigestPacking(ctx, destinationPacked)
				require.NoError(t, err)
				_, err = destination.CopyOrphanedDataFrom(ctx, source.Path())
				require.NoError(t, err)
				got, err := destination.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: "visible"})
				require.NoError(t, err)
				require.Len(t, got.Changes, 1)
				assert.Equal(t, before.Changes[0].MessageID, got.Changes[0].MessageID)
				assert.Equal(t, abcDigest, got.Changes[0].Digest)
				var kind string
				require.NoError(t, destination.getReader().QueryRowContext(ctx, `SELECT typeof(digest) FROM conversation_messages`).Scan(&kind))
				if destinationPacked {
					assert.Equal(t, "blob", kind)
				} else {
					assert.Equal(t, "text", kind)
				}
			})
		}
	}
}

func TestDigestPackingCompactionPreservesExportsAndFutureWrites(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "chat", Role: "assistant", Content: "hello", DialogueText: new("abc"), SourceUUID: "native"}}))
	before, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	_, err = d.SetConversationDigestPacking(ctx, true)
	require.NoError(t, err)
	_, err = d.Compact(ctx, CompactOptions{StagingDir: t.TempDir()})
	require.NoError(t, err)
	after, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	assert.Equal(t, before, after)
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "future", SourceUUID: "next"}}))
	var kind string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT typeof(digest) FROM conversation_messages WHERE ordinal=1`).Scan(&kind))
	assert.Equal(t, "blob", kind)
}

func TestDigestPackingVersionAndMetadataFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		mode    string
		want    string
	}{
		{"unmarked", dataVersion, "v1", "disagrees"},
		{"missing marker", dataVersion + archiveFormatDigestPacking, "", "disagrees"},
		{"unknown mode", dataVersion + archiveFormatDigestPacking, "v99", "unsupported archive storage format"},
		{"empty mode", dataVersion, "", "unsupported archive storage format"},
		{"negative sign bit", -2147483648, "", "negative"},
		{"retired prototype 21", dataVersion + (1 << 21), "", "newer"},
		{"retired prototype 22", dataVersion + (1 << 22), "", "newer"},
		{"future", dataVersion + (1 << 26), "", "newer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				if tc.mode != "" || tc.name == "empty mode" {
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO archive_metadata(key,value) VALUES('conversation_digest_packing',?)`, tc.mode); err != nil {
						return err
					}
				}
				_, err := tx.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version=%d", tc.version))
				return err
			}))
			path := d.Path()
			require.NoError(t, d.Close())
			_, err := OpenReadOnly(t.Context(), path)
			require.ErrorContains(t, err, tc.want)
			_, err = OpenIsolated(t.Context(), path)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestDigestPackingCanceledWriterWait(t *testing.T) {
	d := testDB(t)
	d.mu.Lock()
	defer d.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := d.SetConversationDigestPacking(ctx, true)
	require.ErrorIs(t, err, context.Canceled)
}

func TestStorageMaintenanceOpenDoesNotInitializeOrUpgrade(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, Session{ID: "cold", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "cold", Role: "assistant", Content: "hello"}}))
	path := d.Path()
	require.NoError(t, d.Close())
	maintenance, err := OpenForStorageMaintenance(ctx, path)
	require.NoError(t, err)
	_, err = maintenance.SetConversationDigestPacking(ctx, true)
	require.NoError(t, err)
	require.NoError(t, maintenance.Close())
	reader, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	_, err = reader.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.ErrorIs(t, err, ErrConversationInitializationRequired)
	require.NoError(t, reader.Close())
	// Only an explicit normal export activates the cold index later.
	active, err := OpenIsolated(ctx, path)
	require.NoError(t, err)
	page, err := active.ExportConversationChanges(ctx, ConversationExportOptions{})
	require.NoError(t, err)
	require.Len(t, page.Changes, 1)
	assert.Equal(t, helloDigest, page.Changes[0].Digest)
	var kind string
	require.NoError(t, active.getReader().QueryRowContext(ctx, `SELECT typeof(digest) FROM conversation_messages`).Scan(&kind))
	assert.Equal(t, "blob", kind)
	require.NoError(t, active.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", dataVersion-1+archiveFormatDigestPacking))
	require.NoError(t, err)
	_, err = OpenForStorageMaintenance(ctx, path)
	require.ErrorContains(t, err, "requires a current archive")
	var version int
	require.NoError(t, raw.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version))
	assert.Equal(t, dataVersion-1+archiveFormatDigestPacking, version)
}

func TestDigestPackingCopyRefusesUnfencedSourceBeforeIdentityChanges(t *testing.T) {
	source := testDB(t)
	destination := testDB(t)
	ctx := t.Context()
	var before string
	require.NoError(t, destination.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='archive_id'`).Scan(&before))
	require.NoError(t, source.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES('conversation_digest_packing','v1')`)
		return err
	}))
	err := destination.CopyArchiveIdentityFrom(ctx, source.Path())
	require.ErrorContains(t, err, "metadata disagrees")
	err = destination.CopySessionMetadataFrom(ctx, source.Path())
	require.ErrorContains(t, err, "metadata disagrees")
	_, err = destination.CopyOrphanedDataFrom(ctx, source.Path())
	require.ErrorContains(t, err, "metadata disagrees")
	var after string
	require.NoError(t, destination.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='archive_id'`).Scan(&after))
	assert.Equal(t, before, after)
}
