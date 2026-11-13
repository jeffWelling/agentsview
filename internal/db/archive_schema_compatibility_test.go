package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var newerArchiveSchemaCases = []struct {
	name, ddl, layout string
}{
	{"conversation-usage", `ALTER TABLE conversation_messages ADD COLUMN usage_key TEXT;
ALTER TABLE conversation_messages ADD COLUMN usage_revision INTEGER NOT NULL DEFAULT 0;
UPDATE conversation_messages SET usage_key='saved usage fingerprint',usage_revision=42`, "conversation_messages.usage_key"},
	{"session-usage-total", `CREATE TABLE session_usage_totals (
session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
source TEXT NOT NULL,subagent_scope TEXT NOT NULL DEFAULT '',cost_microdollars INTEGER,
incomplete INTEGER NOT NULL DEFAULT 0,models_json TEXT NOT NULL DEFAULT '[]');
INSERT INTO session_usage_totals VALUES('retained','native','included',123,0,'[]')`, "session_usage_totals"},
	{"diagnostics", `CREATE TABLE diagnostic_events (
id TEXT PRIMARY KEY,source TEXT NOT NULL,message TEXT NOT NULL,
occurred_at TEXT NOT NULL,accepted_at TEXT NOT NULL);
INSERT INTO diagnostic_events VALUES('saved-event','fixture','retained diagnostic','2026-01-01','2026-01-02')`, "diagnostic_events"},
	{"image-text-cache", `CREATE TABLE image_text_cache (
id INTEGER PRIMARY KEY,image_hash TEXT NOT NULL,processor TEXT NOT NULL,
provider TEXT NOT NULL,model TEXT NOT NULL,revision TEXT NOT NULL,
text TEXT NOT NULL DEFAULT '',succeeded INTEGER NOT NULL DEFAULT 0,
failures INTEGER NOT NULL DEFAULT 0,retry_at TEXT NOT NULL DEFAULT '',
updated_at TEXT NOT NULL DEFAULT '',UNIQUE(image_hash,processor));
INSERT INTO image_text_cache(id,image_hash,processor,provider,model,revision,text)
VALUES(-1,'retained-image','ocr','fixture','fixture','1','retained image text')`, "image_text_cache"},
	{"image-text-occurrences", `CREATE TABLE image_text_occurrences (
session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
call_id INTEGER,event_id INTEGER,image_index INTEGER NOT NULL,image_hash TEXT NOT NULL)`, "image_text_occurrences"},
	{"frustration-markers", `CREATE TABLE session_frustration_markers (
session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
transcript_key TEXT NOT NULL,marker_version INTEGER NOT NULL,count INTEGER NOT NULL);
INSERT INTO session_frustration_markers VALUES('retained','saved-transcript',1,7)`, "session_frustration_markers"},
}

// These are legal additive layouts from newer builds. A parser version stays
// old while a preservation rebuild is pending; schema additions are committed
// before that rebuild. The fixture does not damage or remove existing schema.
func newerArchiveSchemaFixture(t *testing.T, ddl string) *DB {
	t.Helper()
	d := testDB(t)
	insertSession(t, d, "retained", "fixture")
	insertMessages(t, d, Message{SessionID: "retained", Role: "assistant", Content: "Retained message"})
	_, err := d.ExportConversationChanges(t.Context(), ConversationExportOptions{})
	require.NoError(t, err)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), ddl)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `UPDATE conversation_messages SET body='retained historical body';
PRAGMA user_version=130`)
		return err
	}))
	return d
}

func archiveSchemaFileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return sha256.Sum256(data)
}

func requireNewerArchiveSchema(t *testing.T, err error, layout string) {
	t.Helper()
	var newer *ArchiveSchemaTooNewError
	require.ErrorAs(t, err, &newer)
	assert.Equal(t, layout, newer.Layout)
	assert.Contains(t, err.Error(), "newer compatible AgentsView build")
}

func TestArchiveSchemaCompatibilityRefusesOpenBeforeInitialization(t *testing.T) {
	for _, tc := range newerArchiveSchemaCases {
		t.Run(tc.name, func(t *testing.T) {
			d := newerArchiveSchemaFixture(t, tc.ddl)
			path := d.Path()
			require.NoError(t, d.Close())
			before := archiveSchemaFileHash(t, path)
			requireNewerArchiveSchema(t, CheckDataVersion(t.Context(), path), tc.layout)
			for _, open := range []struct {
				name string
				run  func(context.Context, string) (*DB, error)
			}{{"read-only", OpenReadOnly}, {"writable", OpenIsolated}} {
				t.Run(open.name, func(t *testing.T) {
					opened, err := open.run(t.Context(), path)
					if opened != nil {
						require.NoError(t, opened.Close())
					}
					requireNewerArchiveSchema(t, err, tc.layout)
					assert.Nil(t, opened)
					assert.Equal(t, before, archiveSchemaFileHash(t, path), "refusal must precede writable initialization")
				})
			}
		})
	}
}

func TestArchiveSchemaCompatibilityRefusesEveryArchiveCopy(t *testing.T) {
	for _, tc := range newerArchiveSchemaCases {
		t.Run(tc.name, func(t *testing.T) {
			source := newerArchiveSchemaFixture(t, tc.ddl)
			path := source.Path()
			require.NoError(t, source.Close())
			sourceBefore := archiveSchemaFileHash(t, path)
			dest := testDB(t)
			insertSession(t, dest, "destination", "fixture")
			insertMessages(t, dest, Message{SessionID: "destination", Role: "user", Content: "Do not replace"})
			require.NoError(t, dest.CheckpointWALTruncateWithRetry(t.Context()))
			destBefore := archiveSchemaFileHash(t, dest.Path())
			copies := []struct {
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
				{"sync-state", dest.CopySyncStateFrom},
				{"commit-trailers", dest.CopyCommitTrailersFrom},
				{"exclusions", dest.CopyExcludedSessionsFrom},
				{"attached-state", dest.CopyAttachedArchiveStateFrom},
				{"source-baselines", func(ctx context.Context, path string) error {
					return dest.CopySessionSourceOwnershipBaselinesFrom(ctx, path, []SessionSourceOwnership{{ID: "retained", Machine: "fixture", Agent: "claude", FilePath: "retained.jsonl"}})
				}},
				{"worktree-mappings", dest.CopyWorktreeProjectMappingsFrom},
				{"insights", dest.CopyInsightsFrom},
				{"friction", dest.CopyFrictionStateFrom},
				{"pricing", dest.CopyModelPricingFrom},
				{"recall", dest.CopyRecallEntriesFrom},
			}
			for _, copy := range copies {
				t.Run(copy.name, func(t *testing.T) {
					requireNewerArchiveSchema(t, copy.run(t.Context(), path), tc.layout)
					require.NoError(t, dest.CheckpointWALTruncateWithRetry(t.Context()))
					assert.Equal(t, sourceBefore, archiveSchemaFileHash(t, path), "source archive must remain intact")
					assert.Equal(t, destBefore, archiveSchemaFileHash(t, dest.Path()), "reject before copying any destination state")
				})
			}
		})
	}
}

func TestArchiveSchemaCompatibilityAllowsOlderSupportedLayout(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `ALTER TABLE conversation_messages ADD COLUMN local_note TEXT;
PRAGMA user_version=130`)
		return err
	}))
	path := d.Path()
	require.NoError(t, d.Close())
	require.NoError(t, CheckDataVersion(t.Context(), path))
	opened, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	defer opened.Close()
	assert.True(t, opened.NeedsResync(), "ordinary older parser data remains eligible for its preservation rebuild")
}

// SQLite identifiers are case-insensitive, and retained state can be exposed
// through a view. Both still identify a layout this build cannot preserve.
func TestArchiveSchemaCompatibilityRecognizesSQLiteIdentifiers(t *testing.T) {
	for _, tc := range []struct{ name, ddl, layout string }{
		{"column", `ALTER TABLE conversation_messages ADD COLUMN Usage_Revision INTEGER NOT NULL DEFAULT 0`, "conversation_messages.Usage_Revision"},
		{"table", `CREATE TABLE Diagnostic_Events(id TEXT PRIMARY KEY)`, "Diagnostic_Events"},
		{"view", `CREATE VIEW Image_Text_Cache AS SELECT 1 AS id`, "Image_Text_Cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newerArchiveSchemaFixture(t, tc.ddl)
			path := d.Path()
			require.NoError(t, d.Close())
			before := archiveSchemaFileHash(t, path)
			requireNewerArchiveSchema(t, CheckDataVersion(t.Context(), path), tc.layout)
			assert.Equal(t, before, archiveSchemaFileHash(t, path))
		})
	}
}

func TestArchiveSchemaCompatibilityRefusesCompactOnExistingHandle(t *testing.T) {
	for _, tc := range newerArchiveSchemaCases {
		t.Run(tc.name, func(t *testing.T) {
			d := newerArchiveSchemaFixture(t, tc.ddl)
			require.NoError(t, d.CheckpointWALTruncateWithRetry(t.Context()))
			before := archiveSchemaFileHash(t, d.Path())
			staging := t.TempDir()
			_, err := d.Compact(t.Context(), CompactOptions{StagingDir: staging})
			requireNewerArchiveSchema(t, err, tc.layout)
			require.NoError(t, d.CheckpointWALTruncateWithRetry(t.Context()))
			assert.Equal(t, before, archiveSchemaFileHash(t, d.Path()))
			entries, err := os.ReadDir(staging)
			require.NoError(t, err)
			assert.Empty(t, entries, "refuse before creating staging or recovery state")
		})
	}
}
