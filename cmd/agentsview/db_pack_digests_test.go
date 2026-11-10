package main

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestDBPackDigestsCommandRoundTrip(t *testing.T) {
	testDataDir(t)
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "chat", Agent: "claude", Project: "sample", Machine: "local"}))
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: "chat", Role: "assistant", Content: "hello", DialogueText: new("abc"), SourceUUID: "native"}}))
	before, err := database.ExportConversationChanges(t.Context(), db.ConversationExportOptions{})
	require.NoError(t, err)
	require.NoError(t, database.Close())
	for _, unpack := range []bool{false, true} {
		cmd := newDBCommand()
		cmd.SetContext(t.Context())
		args := []string{"pack-digests", "--format", "json"}
		if unpack {
			args = append(args, "--unpack")
		}
		cmd.SetArgs(args)
		var output bytes.Buffer
		cmd.SetOut(&output)
		require.NoError(t, cmd.Execute())
		var result db.DigestPackingResult
		require.NoError(t, json.Unmarshal(output.Bytes(), &result))
		assert.Equal(t, !unpack, result.Enabled)
		assert.EqualValues(t, 1, result.ChangedRows)
		reader, err := db.OpenReadOnly(t.Context(), cfg.DBPath)
		require.NoError(t, err)
		after, err := reader.ExportConversationChanges(t.Context(), db.ConversationExportOptions{})
		require.NoError(t, err)
		assert.Equal(t, before, after)
		require.NoError(t, reader.Close())
	}
}

func TestDBPackDigestsRefusesWriterOwner(t *testing.T) {
	testDataDir(t)
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	lock, err := acquireWriteOwnerLock(t.Context(), writeLockDataDir(cfg))
	require.NoError(t, err)
	defer lock.Close()
	_, err = runDBPackDigests(t.Context(), cfg, true)
	require.Error(t, err)
	raw, err := sql.Open("sqlite3", cfg.DBPath)
	require.NoError(t, err)
	defer raw.Close()
	var version int
	require.NoError(t, raw.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version))
	assert.Equal(t, db.CurrentDataVersion(), version, "owner refusal happens before format conversion")
}
