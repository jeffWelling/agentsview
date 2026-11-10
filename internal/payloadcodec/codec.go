// Package payloadcodec encodes individual archive text values and recovers them
// without expanding an archive. It does not enable an on-disk archive format.
package payloadcodec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/agentsview/internal/ctxio"
)

const (
	// ChunkBytes bounds one noninterruptible codec operation. Each chunk is an
	// independent frame: neither decoding nor recovery needs a whole-value window.
	ChunkBytes       = 64 << 10
	MaxDecodedBytes  = 64 << 20
	MaxConcurrency   = 4
	headerBytes      = 48
	chunkHeaderBytes = 8
	maxFrameBytes    = ChunkBytes + 1024
	MaxEncodedBytes  = headerBytes + MaxDecodedBytes + (MaxDecodedBytes/ChunkBytes)*(chunkHeaderBytes+1024)
	minCompressBytes = 1024
)

const magic = "AVPZ0001"

var (
	ErrInvalid   = errors.New("invalid encoded payload")
	ErrLimit     = errors.New("payload exceeds byte limit")
	ErrValueType = errors.New("payload must be NULL, TEXT or an encoded BLOB")
)

// Info describes one envelope. Inspect checks its structure, but only Recover
// and DecodeValue verify decompressed bytes and both levels of checksums.
type Info struct {
	DecodedBytes uint64 `json:"decoded_bytes"`
	EncodedBytes uint64 `json:"encoded_bytes"`
	Chunks       int    `json:"chunks"`
	Verified     bool   `json:"verified"`
}

type envelope struct {
	info   Info
	digest [sha256.Size]byte
}

type worker struct {
	encoder *zstd.Encoder
	decoder *zstd.Decoder
	encoded []byte
	decoded []byte
}

// Codec owns a fixed number of reusable workers. It starts no goroutines and
// has no external resources to close. A caller should share one instance across
// its storage boundary, rather than construct one per value or connection.
type Codec struct{ workers chan *worker }

func New(concurrency int) (*Codec, error) {
	if concurrency < 1 || concurrency > MaxConcurrency {
		return nil, fmt.Errorf("payload concurrency must be between 1 and %d", MaxConcurrency)
	}
	c := &Codec{workers: make(chan *worker, concurrency)}
	for range concurrency {
		c.workers <- nil
	}
	return c, nil
}

func newWorker() (*worker, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(ChunkBytes), zstd.WithSingleSegment(true), zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(ChunkBytes), zstd.WithDecoderMaxWindow(ChunkBytes), zstd.WithDecodeAllCapLimit(true), zstd.WithDecodeBuffersBelow(0))
	if err != nil {
		_ = enc.Close()
		return nil, err
	}
	return &worker{encoder: enc, decoder: dec, encoded: make([]byte, 0, maxFrameBytes), decoded: make([]byte, 0, ChunkBytes)}, nil
}

func (c *Codec) acquire(ctx context.Context) (*worker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil || c.workers == nil {
		return nil, errors.New("payload codec is not initialized")
	}
	select {
	case w := <-c.workers:
		if err := ctx.Err(); err != nil {
			c.workers <- w
			return nil, err
		}
		if w != nil {
			return w, nil
		}
		created, err := newWorker()
		if err != nil {
			c.workers <- nil
			return nil, err
		}
		return created, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Encode returns the original string unless encoding saves at least 32 bytes
// and one eighth of its size. Small and oversized values stay ordinary TEXT;
// this eligibility limit must never truncate or reject existing large text.
// Each encoded chunk has a checksum, and the envelope hashes the entire value.
func (c *Codec) Encode(ctx context.Context, value string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(value) < minCompressBytes || len(value) > MaxDecodedBytes {
		return value, nil
	}
	w, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { c.workers <- w }()
	result := make([]byte, headerBytes)
	copy(result, magic)
	binary.LittleEndian.PutUint64(result[8:16], uint64(len(value)))
	digest := sha256.New()
	threshold := len(value) - max(32, len(value)/8)
	for start := 0; start < len(value); start += ChunkBytes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := w.decoded[:min(ChunkBytes, len(value)-start)]
		copy(chunk, value[start:start+len(chunk)])
		_, _ = digest.Write(chunk)
		frame := w.encoder.EncodeAll(chunk, w.encoded[:0])
		if len(frame) > maxFrameBytes {
			return nil, fmt.Errorf("%w: encoded chunk too large", ErrLimit)
		}
		if len(result)+chunkHeaderBytes+len(frame) > threshold {
			return value, nil
		}
		result = binary.LittleEndian.AppendUint32(result, uint32(len(chunk)))
		result = binary.LittleEndian.AppendUint32(result, uint32(len(frame)))
		result = append(result, frame...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	copy(result[16:headerBytes], digest.Sum(nil))
	return result, nil
}

// DecodeValue is an explicit SQLite storage boundary. NULL remains NULL and
// TEXT returns directly, without acquiring a worker or copying its bytes.
// BLOBs must use this package's envelope. An error returns no partial value.
// maxBytes limits logical output, including ordinary TEXT; it is checked before
// allocating output and again by every frame's hard output-cap limit.
func (c *Codec) DecodeValue(ctx context.Context, value any, maxBytes uint64) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		if uint64(len(v)) > maxBytes {
			return nil, ErrLimit
		}
		return value, nil
	case []byte:
		if len(v) > MaxEncodedBytes {
			return nil, ErrLimit
		}
		source := bytes.NewReader(v)
		h, err := readHeader(ctx, source, maxBytes)
		if err != nil {
			return nil, err
		}
		w, err := c.acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { c.workers <- w }()
		var output strings.Builder
		// The validated envelope length and caller limit bound the final string.
		// No decoder allocation can be based on an untrusted whole-frame size.
		output.Grow(int(h.info.DecodedBytes))
		if _, err := recoverValue(ctx, w, &output, source, h); err != nil {
			return nil, err
		}
		return output.String(), nil
	default:
		return nil, ErrValueType
	}
}

// Recover verifies and streams one encoded value. Its reader/writer must make
// progress or honor cancellation themselves; context checks cannot interrupt an
// arbitrary blocked I/O implementation. On corruption or cancellation dst may
// contain a prefix, so durable callers must stage output and publish only after
// success. Codec scratch is fixed; caller-owned output is not retained.
func (c *Codec) Recover(ctx context.Context, dst io.Writer, src io.Reader, maxBytes uint64) (Info, error) {
	h, err := readHeader(ctx, src, maxBytes)
	if err != nil {
		return Info{}, err
	}
	w, err := c.acquire(ctx)
	if err != nil {
		return Info{}, err
	}
	defer func() { c.workers <- w }()
	return recoverValue(ctx, w, dst, src, h)
}

func recoverValue(ctx context.Context, w *worker, dst io.Writer, src io.Reader, h envelope) (Info, error) {
	digest := sha256.New()
	info, err := walkFrames(ctx, src, h, w.encoded[:maxFrameBytes], func(frame []byte, decoded int) error {
		// Both the frame header and this cap are caller-bounded. A forged envelope
		// advertising one byte cannot make an 8 MiB frame allocate its output.
		result, err := w.decoder.DecodeAll(frame, w.decoded[:0:decoded])
		if err != nil {
			return fmt.Errorf("%w: chunk decode: %v", ErrInvalid, err)
		}
		if len(result) != decoded {
			return fmt.Errorf("%w: chunk length", ErrInvalid)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _ = digest.Write(result)
		n, err := (ctxio.Writer{Context: ctx, Writer: dst}).Write(result)
		if err != nil {
			return err
		}
		if n != len(result) {
			return io.ErrShortWrite
		}
		return nil
	})
	if err != nil {
		return Info{}, err
	}
	if !digestMatches(digest, h.digest) {
		return Info{}, fmt.Errorf("%w: value checksum", ErrInvalid)
	}
	info.Verified = true
	return info, nil
}

func digestMatches(h hash.Hash, expected [sha256.Size]byte) bool {
	var actual [sha256.Size]byte
	h.Sum(actual[:0])
	return actual == expected
}

// Inspect reads a single envelope with bounded scratch and verifies framing and
// declared lengths. It deliberately does not claim to verify content checksums.
func Inspect(ctx context.Context, src io.Reader, maxBytes uint64) (Info, error) {
	h, err := readHeader(ctx, src, maxBytes)
	if err != nil {
		return Info{}, err
	}
	return walkFrames(ctx, src, h, nil, nil)
}

func readHeader(ctx context.Context, src io.Reader, maxBytes uint64) (envelope, error) {
	var raw [headerBytes]byte
	if _, err := io.ReadFull(ctxio.Reader{Context: ctx, Reader: src}, raw[:]); err != nil {
		return envelope{}, inputError(ctx, "envelope header", err)
	}
	if string(raw[:8]) != magic {
		return envelope{}, fmt.Errorf("%w: envelope version", ErrInvalid)
	}
	size := binary.LittleEndian.Uint64(raw[8:16])
	if size > MaxDecodedBytes || size > maxBytes {
		return envelope{}, ErrLimit
	}
	h := envelope{info: Info{DecodedBytes: size, EncodedBytes: headerBytes}}
	copy(h.digest[:], raw[16:])
	return h, nil
}

func walkFrames(ctx context.Context, src io.Reader, h envelope, frame []byte, visit func([]byte, int) error) (Info, error) {
	reader := &ctxio.Reader{Context: ctx, Reader: src}
	var chunkHeader [chunkHeaderBytes]byte
	if frame == nil {
		frame = make([]byte, maxFrameBytes)
	}
	remaining := h.info.DecodedBytes
	for remaining > 0 {
		if _, err := io.ReadFull(reader, chunkHeader[:]); err != nil {
			return Info{}, inputError(ctx, "chunk header", err)
		}
		decoded := binary.LittleEndian.Uint32(chunkHeader[:4])
		encoded := binary.LittleEndian.Uint32(chunkHeader[4:])
		expected := min(remaining, ChunkBytes)
		if uint64(decoded) != expected || encoded == 0 || encoded > maxFrameBytes {
			return Info{}, fmt.Errorf("%w: chunk bounds", ErrInvalid)
		}
		part := frame[:encoded]
		if _, err := io.ReadFull(reader, part); err != nil {
			return Info{}, inputError(ctx, "chunk body", err)
		}
		if err := validateFrame(part, expected); err != nil {
			return Info{}, err
		}
		if visit != nil {
			if err := visit(part, int(decoded)); err != nil {
				return Info{}, err
			}
		}
		h.info.Chunks++
		h.info.EncodedBytes += chunkHeaderBytes + uint64(encoded)
		remaining -= expected
	}
	var trailing [1]byte
	n, err := reader.Read(trailing[:])
	if ctx.Err() != nil {
		return Info{}, ctx.Err()
	}
	if n != 0 {
		return Info{}, fmt.Errorf("%w: trailing bytes", ErrInvalid)
	}
	if err != io.EOF {
		if err == nil {
			err = io.ErrNoProgress
		}
		return Info{}, inputError(ctx, "end of value", err)
	}
	return h.info, nil
}

func validateFrame(frame []byte, expected uint64) error {
	var h zstd.Header
	if err := h.Decode(frame); err != nil {
		return fmt.Errorf("%w: frame header: %v", ErrInvalid, err)
	}
	if h.Skippable || !h.HasFCS || h.FrameContentSize != expected || h.DictionaryID != 0 || !h.HasCheckSum || h.WindowSize > ChunkBytes {
		return fmt.Errorf("%w: frame contract", ErrInvalid)
	}
	// DecodeAll accepts concatenated frames. The envelope instead requires one
	// complete checksummed frame per chunk, including rejection of empty trailers.
	pos := h.HeaderSize
	for {
		if len(frame)-pos < 3 {
			return fmt.Errorf("%w: block header", ErrInvalid)
		}
		block := uint32(frame[pos]) | uint32(frame[pos+1])<<8 | uint32(frame[pos+2])<<16
		pos += 3
		size := int(block >> 3)
		switch (block >> 1) & 3 {
		case 1:
			size = 1
		case 3:
			return fmt.Errorf("%w: reserved block type", ErrInvalid)
		}
		if size > len(frame)-pos {
			return fmt.Errorf("%w: block bounds", ErrInvalid)
		}
		pos += size
		if block&1 != 0 {
			break
		}
	}
	if len(frame)-pos != 4 {
		return fmt.Errorf("%w: frame trailer", ErrInvalid)
	}
	return nil
}

func inputError(ctx context.Context, part string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w: %s: %w", ErrInvalid, part, err)
}
