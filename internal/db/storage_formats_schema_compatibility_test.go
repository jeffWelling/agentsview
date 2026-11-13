package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageFormatsRejectNewerUsageSchema(t *testing.T) {
	for _, mode := range []struct {
		name          string
		refs, digests bool
		version       int
	}{
		{"plain", false, false, 167},
		{"refs", true, false, 167 | archiveFormatSessionRefs},
		{"digests", false, true, 167 | archiveFormatDigestPacking},
		{"both", true, true, 167 | archiveFormatSessionRefs | archiveFormatDigestPacking},
	} {
		t.Run(mode.name, func(t *testing.T) {
			source := testDB(t)
			insertSession(t, source, "retained", "fixture")
			insertMessages(t, source, Message{SessionID: "retained", Ordinal: 0, Role: "assistant", Content: "Retained message"})
			_, err := source.ExportConversationChanges(t.Context(), ConversationExportOptions{})
			require.NoError(t, err)
			if mode.refs {
				_, err = source.SetConversationSessionRefs(t.Context(), true)
				require.NoError(t, err)
			}
			if mode.digests {
				_, err = source.SetConversationDigestPacking(t.Context(), true)
				require.NoError(t, err)
			}
			storageFormatsRequirePhysical(t, source, mode.refs, mode.digests, false)
			var version int
			require.NoError(t, source.Reader().QueryRow(t.Context(), "PRAGMA user_version").Scan(&version))
			require.Equal(t, mode.version, version, "the parser header remains supported while both format flags are valid")

			table := "conversation_messages"
			if mode.refs {
				table = "conversation_message_refs"
			}
			// A pending newer upgrade can retain the supported header. With
			// references enabled these fields are hidden by the logical view;
			// checking only its 16 projected fields would miss retained state.
			require.NoError(t, source.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), "ALTER TABLE "+table+" ADD COLUMN usage_key TEXT; ALTER TABLE "+table+" ADD COLUMN usage_revision INTEGER NOT NULL DEFAULT 0")
				if err != nil {
					return err
				}
				_, err = tx.ExecContext(t.Context(), "UPDATE "+table+" SET usage_key=?,usage_revision=42", "retained\x00usage identity")
				return err
			}))
			var key string
			var revision int
			require.NoError(t, source.Reader().QueryRow(t.Context(), "SELECT usage_key,usage_revision FROM "+table).Scan(&key, &revision))
			require.Equal(t, "retained\x00usage identity", key)
			require.Equal(t, 42, revision)
			path := source.Path()
			require.NoError(t, source.Close())
			before := archiveSchemaFileHash(t, path)
			layout := table + ".usage_key"
			for _, open := range []struct {
				name string
				run  func(context.Context, string) (*DB, error)
			}{
				{"read-only", OpenReadOnly},
				{"writable", OpenIsolated},
				{"maintenance", OpenForStorageMaintenance},
			} {
				t.Run(open.name, func(t *testing.T) {
					opened, err := open.run(t.Context(), path)
					if opened != nil {
						require.NoError(t, opened.Close())
					}
					requireNewerArchiveSchema(t, err, layout)
					assert.Nil(t, opened)
					assert.Equal(t, before, archiveSchemaFileHash(t, path))
				})
			}

			dest := testDB(t)
			insertSession(t, dest, "destination", "fixture")
			insertMessages(t, dest, Message{SessionID: "destination", Ordinal: 0, Role: "user", Content: "Keep destination"})
			require.NoError(t, dest.CheckpointWALTruncateWithRetry(t.Context()))
			destBefore := archiveSchemaFileHash(t, dest.Path())
			for _, copy := range []struct {
				name string
				run  func(context.Context, string) error
			}{
				{"identity", dest.CopyArchiveIdentityFrom},
				{"orphans", func(ctx context.Context, path string) error {
					_, err := dest.CopyOrphanedDataFrom(ctx, path)
					return err
				}},
				{"trash", func(ctx context.Context, path string) error {
					_, err := dest.CopyTrashedDataFrom(ctx, path)
					return err
				}},
				{"metadata", dest.CopySessionMetadataFrom},
			} {
				t.Run(copy.name, func(t *testing.T) {
					requireNewerArchiveSchema(t, copy.run(t.Context(), path), layout)
					require.NoError(t, dest.CheckpointWALTruncateWithRetry(t.Context()))
					assert.Equal(t, before, archiveSchemaFileHash(t, path), "source format and usage state stay intact")
					assert.Equal(t, destBefore, archiveSchemaFileHash(t, dest.Path()), "refuse before destination identity, data or format mutation")
				})
			}
		})
	}
}
