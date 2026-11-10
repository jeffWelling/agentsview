package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/payloadcodec"
)

func recoveryFixture(t *testing.T) (string, []byte, string) {
	t.Helper()
	text := strings.Repeat("recover exact \x00 bytes 世界\xff\n", 10_000)
	codec, err := payloadcodec.New(1)
	require.NoError(t, err)
	value, err := codec.Encode(t.Context(), text)
	require.NoError(t, err)
	blob, ok := value.([]byte)
	require.True(t, ok)
	input := filepath.Join(t.TempDir(), "detached.bin")
	require.NoError(t, os.WriteFile(input, blob, 0600))
	return input, blob, text
}

func TestRecoverPublishesVerifiedFileAndRetainsInput(t *testing.T) {
	input, blob, text := recoveryFixture(t)
	output := filepath.Join(t.TempDir(), "recovered.txt")
	var stdout, stderr bytes.Buffer
	require.NoError(t, run(t.Context(), []string{"--input", input, "--output", output}, &stdout, &stderr))
	actual, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, text, string(actual))
	original, err := os.ReadFile(input)
	require.NoError(t, err)
	assert.Equal(t, blob, original)
	entries, err := os.ReadDir(filepath.Dir(output))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "recovered.txt", entries[0].Name())
	assert.Empty(t, stdout.String(), "raw content never goes to the terminal")
}

func TestRecoverNeverReplacesExistingOutput(t *testing.T) {
	input, _, _ := recoveryFixture(t)
	output := filepath.Join(t.TempDir(), "existing.txt")
	require.NoError(t, os.WriteFile(output, []byte("keep original"), 0600))
	err := run(t.Context(), []string{"--input", input, "--output", output}, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	value, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, "keep original", string(value))
}

func TestFailedRecoveryPublishesNothing(t *testing.T) {
	for _, cause := range []string{"checksum", "limit", "cancellation"} {
		t.Run(cause, func(t *testing.T) {
			input, blob, _ := recoveryFixture(t)
			output := filepath.Join(t.TempDir(), "recovered.txt")
			args := []string{"--input", input, "--output", output}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch cause {
			case "checksum":
				blob[16] ^= 1
				require.NoError(t, os.WriteFile(input, blob, 0600))
			case "limit":
				args = append(args, "--max-bytes", "1")
			case "cancellation":
				cancel()
			}
			require.Error(t, run(ctx, args, &bytes.Buffer{}, &bytes.Buffer{}))
			_, err := os.Stat(output)
			assert.ErrorIs(t, err, os.ErrNotExist)
			entries, err := os.ReadDir(filepath.Dir(output))
			require.NoError(t, err)
			assert.Empty(t, entries, "failed verification removes its staging output")
		})
	}
}

func TestInspectionDoesNotClaimChecksumVerification(t *testing.T) {
	input, blob, text := recoveryFixture(t)
	blob[16] ^= 1
	require.NoError(t, os.WriteFile(input, blob, 0600))
	var output bytes.Buffer
	require.NoError(t, run(t.Context(), []string{"--input", input, "--inspect"}, &output, &bytes.Buffer{}))
	var info payloadcodec.Info
	require.NoError(t, json.Unmarshal(output.Bytes(), &info))
	assert.False(t, info.Verified)
	assert.Equal(t, uint64(len(text)), info.DecodedBytes)
	assert.Equal(t, uint64(len(blob)), info.EncodedBytes)
	assert.NotContains(t, output.String(), "recover exact")
}

func TestRejectsAmbiguousInputOptions(t *testing.T) {
	for _, args := range [][]string{nil, {"--input", "value"}, {"--input", "value", "--inspect", "--output", "out"}, {"--input", "value", "--inspect", "extra"}} {
		assert.Error(t, run(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{}))
	}
}
