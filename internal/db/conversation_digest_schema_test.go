package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A supported handle can outlive a schema change. The direct API must refuse
// before installing triggers or accepting an already-disabled no-op, too.
func TestDigestPackingRefusesNewerSchemaOnExistingHandle(t *testing.T) {
	for _, packed := range []bool{false, true} {
		for _, enable := range []bool{false, true} {
			t.Run(fmt.Sprintf("packed=%t/enable=%t", packed, enable), func(t *testing.T) {
				d := testDB(t)
				ctx := t.Context()
				require.NoError(t, d.UpsertSession(ctx, Session{ID: "usage-session", Agent: "claude", Project: "sample", Machine: "local"}))
				require.NoError(t, d.InsertMessages(ctx, []Message{{SessionID: "usage-session", Role: "assistant", Content: "saved text", DialogueText: new("saved text")}}))
				before, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				if packed {
					_, err = d.SetConversationDigestPacking(ctx, true)
					require.NoError(t, err)
				}
				// These additive columns are installed by upstream before the
				// pending parser upgrade necessarily advances user_version.
				_, err = d.getWriter().ExecContext(ctx, `ALTER TABLE conversation_messages ADD COLUMN usage_key TEXT`)
				require.NoError(t, err)
				_, err = d.getWriter().ExecContext(ctx, `ALTER TABLE conversation_messages ADD COLUMN usage_revision INTEGER NOT NULL DEFAULT 0`)
				require.NoError(t, err)
				_, err = d.getWriter().ExecContext(ctx, `UPDATE conversation_messages SET usage_key='saved-usage', usage_revision=37`)
				require.NoError(t, err)
				beforeSchema, err := schemaHash(ctx, d.rawReader())
				require.NoError(t, err)
				var beforeVersion int
				require.NoError(t, d.rawReader().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&beforeVersion))

				result, err := d.SetConversationDigestPacking(ctx, enable)
				var schemaErr *ArchiveSchemaTooNewError
				require.ErrorAs(t, err, &schemaErr)
				assert.Zero(t, result.ScannedRows)
				assert.Zero(t, result.ChangedRows)
				afterSchema, err := schemaHash(ctx, d.rawReader())
				require.NoError(t, err)
				assert.Equal(t, beforeSchema, afterSchema)
				var afterVersion int
				require.NoError(t, d.rawReader().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&afterVersion))
				assert.Equal(t, beforeVersion, afterVersion)
				var usageKey, digestType string
				var usageRevision int
				require.NoError(t, d.rawReader().QueryRowContext(ctx, `SELECT usage_key,usage_revision,typeof(digest) FROM conversation_messages`).Scan(&usageKey, &usageRevision, &digestType))
				assert.Equal(t, "saved-usage", usageKey)
				assert.Equal(t, 37, usageRevision)
				if packed {
					assert.Equal(t, "blob", digestType)
				} else {
					assert.Equal(t, "text", digestType)
				}
				after, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				assert.Equal(t, before, after, "refusal preserves publication state and stored text")
			})
		}
	}
}
