package payloadcodec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

var benchmarkValue any

func benchmarkText(size int) string {
	var text strings.Builder
	for i := 0; text.Len() < size; i++ {
		fmt.Fprintf(&text, "{\"event\":%d,\"path\":\"synthetic/file-%d.go\",\"result\":\"line one\\nline two\\ncompleted\"}\n", i, i%37)
	}
	return text.String()[:size]
}

func BenchmarkPayloadCodec(b *testing.B) {
	for _, size := range []int{128 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			codec := testCodec(b, 2)
			text := benchmarkText(size)
			blob := encodedValue(b, codec, text)
			// This comparison retains the old prototype's codec settings and allocation
			// shape, without its SQLite callbacks, triggers or unsafe format flags.
			// It is a historical algorithm baseline, not a supported recovery format.
			legacyEncoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true), zstd.WithWindowSize(1<<20))
			require.NoError(b, err)
			defer legacyEncoder.Close()
			legacyDecoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(MaxDecodedBytes), zstd.WithDecoderMaxWindow(1<<20))
			require.NoError(b, err)
			defer legacyDecoder.Close()
			legacyEncode := func() []byte {
				encoded := make([]byte, 16, 16+len(text)/2)
				copy(encoded, []byte{'A', 'V', 'T', 'X', 'T', 0, 1, 0})
				binary.LittleEndian.PutUint64(encoded[8:], uint64(len(text)))
				return legacyEncoder.EncodeAll([]byte(text), encoded)
			}
			legacyBlob := legacyEncode()
			var plain any = text
			b.Run("text_native", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchmarkValue, err = codec.DecodeValue(b.Context(), plain, uint64(size))
					require.NoError(b, err)
				}
				b.ReportMetric(float64(size), "stored-bytes")
			})
			b.Run("chunked_decode", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					benchmarkValue, err = codec.DecodeValue(b.Context(), blob, uint64(size))
					require.NoError(b, err)
				}
				b.ReportMetric(float64(len(blob)), "stored-bytes")
			})
			b.Run("legacy_whole_decode", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					var decoded []byte
					decoded, err = legacyDecoder.DecodeAll(legacyBlob[16:], nil)
					require.NoError(b, err)
					benchmarkValue = string(decoded)
				}
				b.ReportMetric(float64(len(legacyBlob)), "stored-bytes")
			})
			b.Run("chunked_recover_discard", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					_, err = codec.Recover(b.Context(), io.Discard, bytes.NewReader(blob), uint64(size))
					require.NoError(b, err)
				}
				b.ReportMetric(float64(len(blob)), "stored-bytes")
			})
			b.Run("chunked_encode", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					benchmarkValue, err = codec.Encode(b.Context(), text)
					require.NoError(b, err)
				}
				b.ReportMetric(float64(len(blob)), "stored-bytes")
			})
			b.Run("legacy_whole_encode", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					benchmarkValue = legacyEncode()
				}
				b.ReportMetric(float64(len(legacyBlob)), "stored-bytes")
			})
		})
	}
}

func BenchmarkRejectForgedFrame(b *testing.B) {
	codec := testCodec(b, 1)
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	require.NoError(b, err)
	defer enc.Close()
	frame := enc.EncodeAll(bytes.Repeat([]byte("x"), 8<<20), nil)
	blob := envelopeFixture(1, frame, 1)
	// Warm the fixed worker. Rejection must not allocate the advertised 8 MiB.
	_, err = codec.DecodeValue(b.Context(), blob, 1)
	require.ErrorIs(b, err, ErrInvalid)
	b.ReportAllocs()
	for b.Loop() {
		_, err = codec.DecodeValue(b.Context(), blob, 1)
		require.ErrorIs(b, err, ErrInvalid)
	}
}
