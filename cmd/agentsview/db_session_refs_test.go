package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestDBSessionRefsCommandPreservesExports(t *testing.T) {
	dataDir := testDataDir(t)
	path := filepath.Join(dataDir, "sessions.db")
	database, err := db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: "chat", Role: "assistant", SourceUUID: "native", Content: "Retained reply", DialogueText: new("Visible reply")}}))
	before, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{})
	require.NoError(t, err)
	visibleBefore, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{TextMode: "visible"})
	require.NoError(t, err)
	require.NoError(t, database.Close())
	for _, restore := range []bool{false, true} {
		cmd := newDBSessionRefsCommand()
		args := []string{"--yes", "--format", "json"}
		if restore {
			args = append(args, "--restore-text")
		}
		cmd.SetArgs(args)
		var output bytes.Buffer
		cmd.SetOut(&output)
		require.NoError(t, cmd.ExecuteContext(t.Context()))
		assert.Contains(t, output.String(), `"rows":1`)
		assert.Contains(t, output.String(), `"changed":true`)
		reader, err := db.OpenReadOnly(t.Context(), path)
		require.NoError(t, err)
		after, err := reader.ExportConversationChanges(t.Context(), db.ConversationExportOptions{})
		require.NoError(t, err)
		assert.Equal(t, before, after, "the CLI must not adopt identities, backfill, or publish unrelated changes")
		visibleAfter, err := reader.ExportConversationChanges(t.Context(), db.ConversationExportOptions{TextMode: "visible"})
		require.NoError(t, err)
		assert.Equal(t, visibleBefore, visibleAfter)
		quiet, err := reader.ExportConversationChanges(t.Context(), db.ConversationExportOptions{Checkpoint: before.Checkpoint})
		require.NoError(t, err)
		assert.Empty(t, quiet.Changes)
		assert.Equal(t, before.Checkpoint, quiet.Checkpoint)
		pinned := before.Changes[0]
		body, err := reader.GetConversationMessage(t.Context(), db.ConversationMessageOptions{DatabaseID: before.DatabaseID, SessionID: "chat", MessageID: pinned.MessageID, Revision: pinned.Revision})
		require.NoError(t, err)
		require.NotNil(t, body.Text)
		assert.Equal(t, "Retained reply", *body.Text)
		require.NoError(t, reader.Close())
	}
}

func TestDBSessionRefsCommandControls(t *testing.T) {
	dir := testDataDir(t)
	cmd := newDBSessionRefsCommand()
	cmd.SetArgs([]string{"--format", "json"})
	require.EqualError(t, cmd.ExecuteContext(t.Context()), "--format json requires --yes for db session-refs")
	cmd = newDBSessionRefsCommand()
	cmd.SetIn(strings.NewReader("n\n"))
	var output bytes.Buffer
	cmd.SetErr(&output)
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.Contains(t, output.String(), "Aborted.")
	assert.NoFileExists(t, filepath.Join(dir, "sessions.db"))
}
