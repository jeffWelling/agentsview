package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversationSessionRefsNewerSchemaRefusesMaintenanceAndNoop(t *testing.T) {
	for _, tc := range newerArchiveSchemaCases {
		t.Run(tc.name, func(t *testing.T) {
			d := newerArchiveSchemaFixture(t, tc.ddl)
			require.NoError(t, d.CheckpointWALTruncateWithRetry(t.Context()))
			before := archiveSchemaFileHash(t, d.Path())
			for _, enabled := range []bool{false, true} {
				_, err := d.SetConversationSessionRefs(t.Context(), enabled)
				requireNewerArchiveSchema(t, err, tc.layout)
			}
			require.NoError(t, d.Close())
			assert.Equal(t, before, archiveSchemaFileHash(t, d.Path()))
			opened, err := OpenForStorageMaintenance(t.Context(), d.Path())
			if opened != nil {
				require.NoError(t, opened.Close())
			}
			requireNewerArchiveSchema(t, err, tc.layout)
			assert.Nil(t, opened)
			assert.Equal(t, before, archiveSchemaFileHash(t, d.Path()))
		})
	}
}

func TestConversationSessionRefsNewerPhysicalSchemaRefusesReverseAndNoop(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "retained", "fixture")
	insertMessages(t, d, Message{SessionID: "retained", Role: "assistant", Content: "Retained message"})
	_, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	_, err = d.SetConversationSessionRefs(t.Context(), true)
	require.NoError(t, err)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `ALTER TABLE conversation_message_refs ADD COLUMN usage_key TEXT;
ALTER TABLE conversation_message_refs ADD COLUMN usage_revision INTEGER NOT NULL DEFAULT 0;
UPDATE conversation_message_refs SET usage_key='saved fingerprint',usage_revision=42`)
		return err
	}))
	require.NoError(t, d.CheckpointWALTruncateWithRetry(t.Context()))
	before := archiveSchemaFileHash(t, d.Path())
	for _, enabled := range []bool{true, false} {
		_, err := d.SetConversationSessionRefs(t.Context(), enabled)
		requireNewerArchiveSchema(t, err, "conversation_message_refs.usage_key")
	}
	require.NoError(t, d.Close())
	assert.Equal(t, before, archiveSchemaFileHash(t, d.Path()))
}
