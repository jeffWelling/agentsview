package main

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/require"
)

func TestDecideCompactRoute(t *testing.T) {
	tests := []struct {
		name       string
		tr         transport
		stagingDir string
		delegate   bool
		wantErr    string
	}{
		{
			name:     "writable daemon delegates",
			tr:       transport{Mode: transportHTTP},
			delegate: true,
		},
		{
			name:       "writable daemon rejects staging dir",
			tr:         transport{Mode: transportHTTP},
			stagingDir: "/tmp/staging",
			wantErr:    "--staging-dir requires direct archive access",
		},
		{
			name: "read-only pg or duckdb server compacts directly",
			tr:   transport{Mode: transportHTTP, ReadOnly: true},
		},
		{
			name: "read-only server allows staging dir",
			tr:   transport{Mode: transportHTTP, ReadOnly: true},
			// A read-only server does not own the SQLite archive, so the
			// direct-only staging flag stays usable without stopping it.
			stagingDir: "/tmp/staging",
		},
		{
			name:    "unreachable writable daemon refuses direct",
			tr:      transport{Mode: transportDirect, DirectReadOnly: true},
			wantErr: "cannot compact directly",
		},
		{
			name: "no daemon compacts directly",
			tr:   transport{Mode: transportDirect},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delegate, err := decideCompactRoute(tt.tr, tt.stagingDir)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.delegate, delegate)
		})
	}
}

func TestDBCompactJSONRequiresYes(t *testing.T) {
	cmd := newDBCompactCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--format", "json"})

	err := cmd.Execute()
	require.EqualError(t, err, "--format json requires --yes for db compact")
	require.Empty(t, stdout.String(), "JSON mode must not mix a prompt into stdout")
}

func TestDBCompactResultJSONNewline(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, writeDBCompactResult(&output, db.CompactResult{}, true))
	var result db.CompactResult
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	require.Equal(t, byte('\n'), output.Bytes()[output.Len()-1])
}

func TestRequestDBCompactRejectsEmptySuccess(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()

	_, err := requestDBCompact(t.Context(), transport{URL: endpoint.URL}, "", db.CompactOptions{})

	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestRequestDBCompactRefusesUnsupportedConsolidation(t *testing.T) {
	called := false
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusOK) }))
	defer endpoint.Close()
	_, err := requestDBCompact(t.Context(), transport{URL: endpoint.URL}, "", db.CompactOptions{ConsolidateMessageIndex: true})
	require.ErrorContains(t, err, "cannot be delegated")
	require.False(t, called)
}

func TestDBCompactConsolidationCommandPreservesArchiveState(t *testing.T) {
	dataDir := testDataDir(t)
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	d, err := db.OpenIsolated(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "retained", Project: "original", Machine: "fixture", Agent: "claude"}))
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{{SessionID: "retained", Ordinal: 0, Role: "assistant", Content: "kept body"}}))
	_, err = d.ExportConversationChanges(t.Context(), db.ConversationExportOptions{Limit: 10})
	require.NoError(t, err)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE conversation_messages SET body='historical body',digest='legacy digest'; INSERT INTO archive_metadata(key,value) VALUES('fixture-checkpoint','opaque cursor')`)
		return err
	}))
	var beforeRevision int64
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT revision FROM conversation_messages WHERE session_id='retained'`).Scan(&beforeRevision))
	require.NoError(t, d.Close())
	cmd := newDBCompactCommand()
	cmd.SetArgs([]string{"--consolidate-message-index", "--yes", "--format", "json", "--staging-dir", filepath.Join(dataDir, "staging")})
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	var result db.CompactResult
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	require.Equal(t, "already_canonical", result.MessageIndexStatus)
	read, err := db.OpenReadOnly(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	defer read.Close()
	var body, digest, cursor, project string
	var revision int64
	require.NoError(t, read.Reader().QueryRow(t.Context(), `SELECT body,digest,revision FROM conversation_messages WHERE session_id='retained'`).Scan(&body, &digest, &revision))
	require.Equal(t, "historical body", body)
	require.Equal(t, "legacy digest", digest)
	require.Equal(t, beforeRevision, revision)
	require.NoError(t, read.Reader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key='fixture-checkpoint'`).Scan(&cursor))
	require.Equal(t, "opaque cursor", cursor)
	require.NoError(t, read.Reader().QueryRow(t.Context(), `SELECT project FROM sessions WHERE id='retained'`).Scan(&project))
	require.Equal(t, "original", project)
}

// Literal old schema: this fixture deliberately begins with both index trees.
const compactCommandLegacyMessages = `CREATE TABLE messages (
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

func TestDBCompactConsolidatesColdLegacyCommandArchive(t *testing.T) {
	testDataDir(t)
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	raw, err := sql.Open("sqlite3", cfg.DBPath)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), compactCommandLegacyMessages+`; CREATE INDEX idx_messages_session_ordinal ON messages(session_id, ordinal)`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	d, err := db.OpenIsolated(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO sessions(id,project) VALUES('cold','retained project'); INSERT INTO messages(id,session_id,ordinal,role,content) VALUES(41,'cold',0,'assistant',?); INSERT INTO pinned_messages(id,session_id,message_id,ordinal,note) VALUES(9,'cold',41,0,'retained pin'); INSERT INTO archive_metadata(key,value) VALUES('fixture-cursor','opaque checkpoint')`, "cold body\x00雪")
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version=%d", db.CurrentDataVersion()))
		return err
	}))
	metadata := func(d *db.DB) map[string]string {
		rows, err := d.Reader().QueryContext(t.Context(), `SELECT key,value FROM archive_metadata`)
		require.NoError(t, err)
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var key, value string
			require.NoError(t, rows.Scan(&key, &value))
			out[key] = value
		}
		require.NoError(t, rows.Err())
		return out
	}
	before := metadata(d)
	require.NotContains(t, before, "conversation_export_initialized")
	var projected int
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT count(*) FROM conversation_messages`).Scan(&projected))
	require.Zero(t, projected)
	require.NoError(t, d.Close())
	cmd := newDBCompactCommand()
	cmd.SetArgs([]string{"--consolidate-message-index", "--yes", "--format", "json", "--keep-backup"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	var result db.CompactResult
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	require.Equal(t, "consolidated", result.MessageIndexStatus)
	require.FileExists(t, result.BackupPath)
	d, err = db.OpenReadOnly(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	defer d.Close()
	require.Equal(t, before, metadata(d))
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT count(*) FROM conversation_messages`).Scan(&projected))
	require.Zero(t, projected)
	var content, note string
	var id int64
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT id,content FROM messages INDEXED BY idx_messages_session_ordinal WHERE session_id='cold' AND ordinal=0`).Scan(&id, &content))
	require.Equal(t, int64(41), id)
	require.Equal(t, "cold body\x00雪", content)
	require.NoError(t, d.Reader().QueryRow(t.Context(), `SELECT note FROM pinned_messages WHERE id=9 AND message_id=41`).Scan(&note))
	require.Equal(t, "retained pin", note)
}
