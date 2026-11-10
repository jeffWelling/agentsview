package payloadcodec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCodec(t testing.TB, concurrency int) *Codec {
	t.Helper()
	c, err := New(concurrency)
	require.NoError(t, err)
	return c
}

func encodedValue(t testing.TB, c *Codec, text string) []byte {
	t.Helper()
	value, err := c.Encode(t.Context(), text)
	require.NoError(t, err)
	blob, ok := value.([]byte)
	require.True(t, ok, "fixture must exercise encoded storage")
	return blob
}

func TestValueBoundaryPreservesTypesAndBytes(t *testing.T) {
	c := testCodec(t, 2)
	for _, tc := range []struct {
		name  string
		value any
		max   uint64
	}{
		{"null", nil, 0}, {"empty", "", 0}, {"plain", "ordinary text", 13},
		{"nul unicode invalid UTF8", "a\x00世界\xff\xfe", 10},
		{"magic is ordinary text", "AVPZ0001 is text", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.DecodeValue(t.Context(), tc.value, tc.max)
			require.NoError(t, err)
			assert.Equal(t, tc.value, got)
		})
	}
	text := strings.Repeat("a\x00世界\xff\xfe repeated exact bytes\n", 10_000)
	blob := encodedValue(t, c, text)
	require.Less(t, len(blob), len(text))
	got, err := c.DecodeValue(t.Context(), blob, uint64(len(text)))
	require.NoError(t, err)
	assert.Equal(t, text, got)
	info, err := Inspect(t.Context(), bytes.NewReader(blob), uint64(len(text)))
	require.NoError(t, err)
	assert.False(t, info.Verified)
	assert.Equal(t, uint64(len(blob)), info.EncodedBytes)
	assert.Greater(t, info.Chunks, 1)
	var output bytes.Buffer
	info, err = c.Recover(t.Context(), &output, bytes.NewReader(blob), uint64(len(text)))
	require.NoError(t, err)
	assert.True(t, info.Verified)
	assert.Equal(t, text, output.String())
	for _, value := range []any{42, true, []byte("ordinary blob")} {
		got, err := c.DecodeValue(t.Context(), value, 1024)
		require.Error(t, err)
		assert.Nil(t, got)
	}
}

func TestEncodeLeavesUnprofitableAndOversizedText(t *testing.T) {
	c := testCodec(t, 1)
	for _, text := range []string{"small", strings.Repeat("x", MaxDecodedBytes+1)} {
		value, err := c.Encode(t.Context(), text)
		require.NoError(t, err)
		assert.Equal(t, text, value)
	}
	// Deterministic pseudorandom bytes keep the fixture self-contained without
	// depending on a compressor output ratio for tiny/obvious strings.
	raw := make([]byte, 8192)
	state := uint32(0x12345678)
	for i := range raw {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		raw[i] = byte(state)
	}
	value, err := c.Encode(t.Context(), string(raw))
	require.NoError(t, err)
	assert.Equal(t, string(raw), value)
}

func TestDecodeEnforcesCallerBudgetAndExactBoundary(t *testing.T) {
	c := testCodec(t, 1)
	text := strings.Repeat("x", ChunkBytes+1)
	blob := encodedValue(t, c, text)
	for _, input := range []any{text, blob} {
		got, err := c.DecodeValue(t.Context(), input, uint64(len(text)-1))
		require.ErrorIs(t, err, ErrLimit)
		assert.Nil(t, got)
		got, err = c.DecodeValue(t.Context(), input, uint64(len(text)))
		require.NoError(t, err)
		assert.Equal(t, text, got)
	}
	oversized := append([]byte(nil), blob...)
	binary.LittleEndian.PutUint64(oversized[8:16], MaxDecodedBytes+1)
	_, err := c.DecodeValue(t.Context(), oversized, MaxDecodedBytes+1)
	assert.ErrorIs(t, err, ErrLimit)
}

func TestMaximumValueRecoversWithoutWholeValueOutput(t *testing.T) {
	c := testCodec(t, 1)
	text := strings.Repeat("x", MaxDecodedBytes)
	blob := encodedValue(t, c, text)
	info, err := c.Recover(t.Context(), io.Discard, bytes.NewReader(blob), MaxDecodedBytes)
	require.NoError(t, err)
	assert.True(t, info.Verified)
	assert.Equal(t, uint64(MaxDecodedBytes), info.DecodedBytes)
	assert.Equal(t, 1024, info.Chunks)
}

// envelopeFixture is deliberately independent of Encode: malformed frames and
// dishonest lengths must be rejected at our storage boundary, not normalized by
// our encoder before they reach it.
func envelopeFixture(size uint64, frame []byte, chunkSize uint32) []byte {
	header := append([]byte("AVPZ0001"), make([]byte, 40)...)
	binary.LittleEndian.PutUint64(header[8:16], size)
	header = binary.LittleEndian.AppendUint32(header, chunkSize)
	header = binary.LittleEndian.AppendUint32(header, uint32(len(frame)))
	return append(header, frame...)
}

func TestForgedTinyEnvelopeCannotDecodeLargeFrame(t *testing.T) {
	c := testCodec(t, 1)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, encoder.Close()) })
	frame := encoder.EncodeAll(bytes.Repeat([]byte("x"), 8<<20), nil)
	// Both the advertised overall size and the chunk size lie. The frame itself
	// advertises 8 MiB. No decoded byte may reach the caller with a one-byte budget.
	blob := envelopeFixture(1, frame, 1)
	var output bytes.Buffer
	_, err = c.Recover(t.Context(), &output, bytes.NewReader(blob), 1)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Zero(t, output.Len())
	value, err := c.DecodeValue(t.Context(), blob, 1)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Nil(t, value)
}

func TestCorruptionNeverReturnsPartialValue(t *testing.T) {
	c := testCodec(t, 1)
	valid := encodedValue(t, c, strings.Repeat("payload", 20_000))
	cases := map[string]func([]byte) []byte{
		"version":              func(v []byte) []byte { v[7] = '2'; return v },
		"whole checksum":       func(v []byte) []byte { v[16] ^= 1; return v },
		"chunk checksum":       func(v []byte) []byte { n := binary.LittleEndian.Uint32(v[52:56]); v[55+n] ^= 1; return v },
		"truncated header":     func(v []byte) []byte { return v[:40] },
		"truncated chunk":      func(v []byte) []byte { return v[:len(v)-1] },
		"trailing byte":        func(v []byte) []byte { return append(v, 0) },
		"chunk length":         func(v []byte) []byte { binary.LittleEndian.PutUint32(v[48:52], 1); return v },
		"encoded chunk length": func(v []byte) []byte { binary.LittleEndian.PutUint32(v[52:56], maxFrameBytes+1); return v },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			value, err := c.DecodeValue(t.Context(), mutate(append([]byte(nil), valid...)), MaxDecodedBytes)
			require.ErrorIs(t, err, ErrInvalid)
			assert.Nil(t, value)
		})
	}
	// An unchecked header inspection must never report the content as verified.
	corrupt := append([]byte(nil), valid...)
	corrupt[16] ^= 1
	info, err := Inspect(t.Context(), bytes.NewReader(corrupt), MaxDecodedBytes)
	require.NoError(t, err)
	assert.False(t, info.Verified)
}

func TestRejectsForeignFrameContracts(t *testing.T) {
	c := testCodec(t, 1)
	for _, tc := range []struct {
		name        string
		checksum    bool
		concatenate bool
	}{
		{"no checksum", false, false}, {"concatenated frames", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(tc.checksum))
			require.NoError(t, err)
			defer enc.Close()
			frame := enc.EncodeAll([]byte("x"), nil)
			if tc.concatenate {
				frame = append(frame, enc.EncodeAll([]byte("y"), nil)...)
			}
			blob := envelopeFixture(1, frame, 1)
			_, err = c.DecodeValue(t.Context(), blob, 1)
			assert.ErrorIs(t, err, ErrInvalid)
		})
	}
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started); <-w.release })
	return len(p), nil
}

func TestConcurrencyWaitIsCancelableAndTextBypassesWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testCodec(t, 1)
		blob := encodedValue(t, c, strings.Repeat("x", ChunkBytes*2))
		blocker := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
		var release sync.Once
		defer release.Do(func() { close(blocker.release) })
		done := make(chan error, 1)
		go func() { _, err := c.Recover(t.Context(), blocker, bytes.NewReader(blob), ChunkBytes*2); done <- err }()
		<-blocker.started
		// Plain text does not wait for a decoder even while every worker is occupied.
		value, err := c.DecodeValue(t.Context(), "ready", 5)
		require.NoError(t, err)
		assert.Equal(t, "ready", value)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		canceled := make(chan error, 1)
		go func() { _, err := c.DecodeValue(ctx, blob, ChunkBytes*2); canceled <- err }()
		// Both goroutines must be durably blocked before cancellation: this covers
		// waiting for a worker, rather than merely a pre-canceled call.
		synctest.Wait()
		cancel()
		assert.ErrorIs(t, <-canceled, context.Canceled)
		release.Do(func() { close(blocker.release) })
		require.NoError(t, <-done)
	})
}

type cancelWriter struct {
	cancel context.CancelFunc
	writes int
}

func (w *cancelWriter) Write(p []byte) (int, error) { w.writes++; w.cancel(); return len(p), nil }

func TestCancellationBetweenChunks(t *testing.T) {
	c := testCodec(t, 1)
	blob := encodedValue(t, c, strings.Repeat("x", ChunkBytes*3))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	output := &cancelWriter{cancel: cancel}
	_, err := c.Recover(ctx, output, bytes.NewReader(blob), ChunkBytes*3)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, output.writes)
	_, err = c.Encode(ctx, "small")
	assert.ErrorIs(t, err, context.Canceled)
	// The canceled operation releases its permit for later readers.
	_, err = c.DecodeValue(t.Context(), blob, ChunkBytes*3)
	require.NoError(t, err)
}

type failureWriter struct {
	err   error
	short bool
}

func (w failureWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, w.err
}

func TestRecoveryPropagatesOutputFailure(t *testing.T) {
	c := testCodec(t, 1)
	blob := encodedValue(t, c, strings.Repeat("x", 4096))
	sentinel := errors.New("output unavailable")
	for _, tc := range []struct {
		out  failureWriter
		want error
	}{
		{failureWriter{err: sentinel}, sentinel}, {failureWriter{short: true}, io.ErrShortWrite},
	} {
		_, err := c.Recover(t.Context(), tc.out, bytes.NewReader(blob), 4096)
		assert.ErrorIs(t, err, tc.want)
	}
}

func FuzzDecodeValue(f *testing.F) {
	c, err := New(1)
	require.NoError(f, err)
	text := strings.Repeat("exact bytes \x00\xff", 200)
	blob, err := c.Encode(f.Context(), text)
	require.NoError(f, err)
	f.Add(blob.([]byte), uint32(len(text)))
	f.Add([]byte("AVPZ0001"), uint32(1))
	f.Fuzz(func(t *testing.T, input []byte, limit uint32) {
		maxBytes := uint64(limit % (ChunkBytes*2 + 1))
		value, err := c.DecodeValue(t.Context(), input, maxBytes)
		if err != nil {
			assert.Nil(t, value)
			return
		}
		decoded, ok := value.(string)
		require.True(t, ok)
		assert.LessOrEqual(t, uint64(len(decoded)), maxBytes)
		digest := sha256.Sum256([]byte(decoded))
		assert.Equal(t, input[16:48], digest[:])
	})
}

func TestDishonestFrameBodyCannotExceedOutputCap(t *testing.T) {
	c := testCodec(t, 1)
	// Single-segment checksummed Zstd frame claims one byte, but its raw block
	// contains 64 KiB. The envelope and frame header agree, so the decoder's
	// cap check (not only the envelope/header preflight) must reject it.
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x24, 1, 1, 0, 8}
	frame = append(frame, bytes.Repeat([]byte("x"), ChunkBytes)...)
	frame = append(frame, 0, 0, 0, 0)
	blob := envelopeFixture(1, frame, 1)
	info, err := Inspect(t.Context(), bytes.NewReader(blob), 1)
	require.NoError(t, err, "structural inspection cannot validate decoded content")
	assert.False(t, info.Verified)
	var output bytes.Buffer
	_, err = c.Recover(t.Context(), &output, bytes.NewReader(blob), 1)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Zero(t, output.Len())
}
