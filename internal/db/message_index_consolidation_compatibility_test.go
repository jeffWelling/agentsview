package db

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageIndexMaintenanceRefusesNewerSchemaOnOpenHandle(t *testing.T) {
	for _, layout := range []string{"diagnostics", "conversation-usage"} {
		for _, operation := range []struct {
			name        string
			legacy      bool
			planOnly    bool
			consolidate bool
		}{
			{name: "ordinary-compact"},
			{name: "convert-legacy", legacy: true, consolidate: true},
			{name: "already-canonical", consolidate: true},
			{name: "plan-legacy", legacy: true, planOnly: true},
			{name: "plan-canonical", planOnly: true},
		} {
			t.Run(layout+"/"+operation.name, func(t *testing.T) {
				var d *DB
				if operation.legacy {
					d = legacyMessageIndexDB(t)
				} else {
					d = testDB(t)
				}
				seedMessageIndexHistory(t, d)
				// A newer upstream writable open can add these real schema
				// objects while retaining the old header until its rebuild
				// finishes. Keep this already-open handle to exercise the
				// maintenance boundary independently of startup validation.
				tables := []string{"messages", "sessions", "conversation_messages", "archive_metadata", "stats"}
				require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
					var query string
					if layout == "diagnostics" {
						query = `CREATE TABLE diagnostic_events (
 id TEXT PRIMARY KEY, source TEXT NOT NULL, message TEXT NOT NULL,
 occurred_at TEXT NOT NULL, accepted_at TEXT NOT NULL);
CREATE INDEX idx_diagnostic_events_occurred ON diagnostic_events(occurred_at);
INSERT INTO diagnostic_events VALUES('retained-diagnostic','fixture','kept evidence','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z');`
					} else {
						query = `ALTER TABLE conversation_messages ADD COLUMN usage_key TEXT;
ALTER TABLE conversation_messages ADD COLUMN usage_revision INTEGER NOT NULL DEFAULT 0;
UPDATE conversation_messages SET usage_key='retained usage identity',usage_revision=19 WHERE message_id='retained-tombstone';`
					}
					_, err := tx.ExecContext(t.Context(), query)
					return err
				}))
				if layout == "diagnostics" {
					tables = append(tables, "diagnostic_events")
				}
				before := make(map[string][][]any)
				for _, table := range tables {
					before[table] = messageIndexRows(t, d.rawReader(), table)
				}
				beforeSchema, err := schemaHash(t.Context(), d.rawReader())
				require.NoError(t, err)
				var beforeVersion int
				require.NoError(t, d.Reader().QueryRow(t.Context(), "PRAGMA user_version").Scan(&beforeVersion))
				staging := t.TempDir()
				if operation.planOnly {
					_, err = planMessageIndexConsolidation(t.Context(), d.rawReader())
				} else {
					_, err = d.Compact(t.Context(), CompactOptions{StagingDir: staging, ConsolidateMessageIndex: operation.consolidate})
				}
				var newer *ArchiveSchemaTooNewError
				require.ErrorAs(t, err, &newer)
				for _, table := range tables {
					assert.Equal(t, before[table], messageIndexRows(t, d.rawReader(), table), table)
				}
				afterSchema, err := schemaHash(t.Context(), d.rawReader())
				require.NoError(t, err)
				assert.Equal(t, beforeSchema, afterSchema)
				var afterVersion int
				require.NoError(t, d.Reader().QueryRow(t.Context(), "PRAGMA user_version").Scan(&afterVersion))
				assert.Equal(t, beforeVersion, afterVersion)
				assert.False(t, d.WriterClosed())
				require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
					var one int
					return tx.QueryRowContext(t.Context(), "SELECT 1").Scan(&one)
				}))
				entries, err := os.ReadDir(staging)
				require.NoError(t, err)
				assert.Empty(t, entries)
				assert.NoFileExists(t, compactManifestPath(d.Path()))
			})
		}
	}
}
