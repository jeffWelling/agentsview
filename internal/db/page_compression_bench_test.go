package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This is an experiment container, NOT a SQLite VFS or supported archive format.
// It measures real framing/index bytes and uncached random-page reconstruction.
// SQLite never opens this representation, and no application code imports it.
const pageExperimentHeader = 24
const pageExperimentEntry = 24

type pageExperimentBlock struct {
	offset           uint64
	stored, raw, crc uint32
	compressed       uint32
}

type pageExperiment struct {
	file                 *os.File
	decoder              *zstd.Decoder
	blocks               []pageExperimentBlock
	pageSize, groupBytes int
	plainBytes           uint64
}

func buildPageExperiment(ctx context.Context, source, target string, pageSize, groupPages int) error {
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 || groupPages < 1 || groupPages > (1<<20)/pageSize {
		return fmt.Errorf("invalid page grouping")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if info.Size() <= 0 || info.Size() > 2<<30 || info.Size()%int64(pageSize) != 0 {
		return fmt.Errorf("experiment requires a page-aligned snapshot of at most 2 GiB")
	}
	groupBytes := pageSize * groupPages
	count := (info.Size() + int64(groupBytes) - 1) / int64(groupBytes)
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderCRC(true), zstd.WithWindowSize(1<<20))
	if err != nil {
		return err
	}
	defer encoder.Close()
	header := make([]byte, pageExperimentHeader)
	copy(header, "AVPGEXP1")
	binary.LittleEndian.PutUint32(header[8:], uint32(pageSize))
	binary.LittleEndian.PutUint32(header[12:], uint32(groupBytes))
	binary.LittleEndian.PutUint64(header[16:], uint64(info.Size()))
	if _, err = out.Write(header); err != nil {
		return err
	}
	offset := uint64(pageExperimentHeader + count*pageExperimentEntry)
	if _, err = out.Seek(int64(offset), io.SeekStart); err != nil {
		return err
	}
	raw := make([]byte, groupBytes)
	var packed []byte
	for i := int64(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int(min(int64(groupBytes), info.Size()-i*int64(groupBytes)))
		if _, err = io.ReadFull(in, raw[:n]); err != nil {
			return err
		}
		packed = encoder.EncodeAll(raw[:n], packed[:0])
		stored, flag := packed, uint32(1)
		if len(packed) >= n {
			stored, flag = raw[:n], 0
		}
		entry := make([]byte, pageExperimentEntry)
		binary.LittleEndian.PutUint64(entry, offset)
		binary.LittleEndian.PutUint32(entry[8:], uint32(len(stored)))
		binary.LittleEndian.PutUint32(entry[12:], uint32(n))
		binary.LittleEndian.PutUint32(entry[16:], crc32.ChecksumIEEE(raw[:n]))
		binary.LittleEndian.PutUint32(entry[20:], flag)
		if _, err = out.WriteAt(entry, pageExperimentHeader+i*pageExperimentEntry); err != nil {
			return err
		}
		if _, err = out.Write(stored); err != nil {
			return err
		}
		offset += uint64(len(stored))
	}
	return out.Sync()
}

func openPageExperiment(ctx context.Context, path string) (*pageExperiment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
		}
	}()
	header := make([]byte, pageExperimentHeader)
	if _, err = io.ReadFull(f, header); err != nil {
		return nil, err
	}
	if string(header[:8]) != "AVPGEXP1" {
		return nil, fmt.Errorf("unknown experiment format")
	}
	p := &pageExperiment{file: f, pageSize: int(binary.LittleEndian.Uint32(header[8:])), groupBytes: int(binary.LittleEndian.Uint32(header[12:])), plainBytes: binary.LittleEndian.Uint64(header[16:])}
	if p.pageSize < 512 || p.pageSize > 65536 || p.pageSize&(p.pageSize-1) != 0 || p.groupBytes < p.pageSize || p.groupBytes > 1<<20 || p.groupBytes%p.pageSize != 0 || p.plainBytes == 0 || p.plainBytes > 2<<30 || p.plainBytes%uint64(p.pageSize) != 0 {
		return nil, fmt.Errorf("invalid experiment dimensions")
	}
	count := (p.plainBytes + uint64(p.groupBytes) - 1) / uint64(p.groupBytes)
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := uint64(pageExperimentHeader) + count*pageExperimentEntry
	if offset > uint64(info.Size()) {
		return nil, io.ErrUnexpectedEOF
	}
	p.blocks = make([]pageExperimentBlock, count)
	entry := make([]byte, pageExperimentEntry)
	for i := range p.blocks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err = io.ReadFull(f, entry); err != nil {
			return nil, err
		}
		b := pageExperimentBlock{binary.LittleEndian.Uint64(entry), binary.LittleEndian.Uint32(entry[8:]), binary.LittleEndian.Uint32(entry[12:]), binary.LittleEndian.Uint32(entry[16:]), binary.LittleEndian.Uint32(entry[20:])}
		wantRaw := min(uint64(p.groupBytes), p.plainBytes-uint64(i)*uint64(p.groupBytes))
		if b.offset != offset || uint64(b.raw) != wantRaw || b.stored == 0 || b.stored > b.raw || b.compressed > 1 || (b.compressed == 0 && b.stored != b.raw) {
			return nil, fmt.Errorf("invalid block %d", i)
		}
		offset += uint64(b.stored)
		if offset > uint64(info.Size()) {
			return nil, io.ErrUnexpectedEOF
		}
		p.blocks[i] = b
	}
	if offset != uint64(info.Size()) {
		return nil, fmt.Errorf("unexpected trailing bytes")
	}
	p.decoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(2<<20), zstd.WithDecoderMaxWindow(1<<20), zstd.WithDecodeAllCapLimit(true))
	if err != nil {
		return nil, err
	}
	ok = true
	return p, nil
}

func (p *pageExperiment) readPage(ctx context.Context, page uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if page >= p.plainBytes/uint64(p.pageSize) {
		return nil, fmt.Errorf("page out of range")
	}
	position := page * uint64(p.pageSize)
	b := p.blocks[position/uint64(p.groupBytes)]
	stored := make([]byte, int(b.stored))
	if _, err := p.file.ReadAt(stored, int64(b.offset)); err != nil {
		return nil, err
	}
	raw := stored
	if b.compressed == 1 {
		var err error
		raw, err = p.decoder.DecodeAll(stored, make([]byte, 0, int(b.raw)))
		if err != nil {
			return nil, err
		}
	}
	if len(raw) != int(b.raw) || crc32.ChecksumIEEE(raw) != b.crc {
		return nil, fmt.Errorf("block content mismatch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	within := position % uint64(p.groupBytes)
	return raw[within : within+uint64(p.pageSize)], nil
}

func pageExperimentFixture(ctx context.Context, tb testing.TB, rows int) (*DB, int) {
	tb.Helper()
	d := testDB(tb)
	rng := rand.New(rand.NewPCG(7, 11))
	require.NoError(tb, d.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE page_experiment_rows(id INTEGER PRIMARY KEY,session_id TEXT NOT NULL,body BLOB NOT NULL); CREATE INDEX page_experiment_session ON page_experiment_rows(session_id)`); err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO page_experiment_rows VALUES(?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < rows; i++ {
			body := bytes.Repeat([]byte("synthetic tool output with repeated fields\n"), 48)
			if i%4 == 0 {
				for j := range body {
					body[j] = byte(rng.Uint64())
				}
			}
			if _, err := stmt.ExecContext(ctx, i, fmt.Sprintf("synthetic-session-%06d", i/100), body); err != nil {
				return err
			}
		}
		return nil
	}))
	_, err := d.getWriter().ExecContext(ctx, "VACUUM")
	require.NoError(tb, err)
	require.NoError(tb, d.CheckpointWALTruncate(ctx))
	var pageSize int
	require.NoError(tb, d.Reader().QueryRow(ctx, "PRAGMA page_size").Scan(&pageSize))
	return d, pageSize
}

func TestPageCompressionExperimentPreservesSnapshot(t *testing.T) {
	d, pageSize := pageExperimentFixture(t.Context(), t, 137)
	want, err := os.ReadFile(d.Path())
	require.NoError(t, err)
	for _, pages := range []int{1, 16} {
		t.Run(fmt.Sprintf("pages_%d", pages), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pages.experiment")
			require.NoError(t, buildPageExperiment(t.Context(), d.Path(), path, pageSize, pages))
			p, err := openPageExperiment(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { p.decoder.Close(); require.NoError(t, p.file.Close()) })
			// Reverse order exercises actual random offsets, including the tail.
			for i := len(want)/pageSize - 1; i >= 0; i-- {
				got, err := p.readPage(t.Context(), uint64(i))
				require.NoError(t, err)
				require.Equal(t, want[i*pageSize:(i+1)*pageSize], got, "page %d", i)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = p.readPage(ctx, 0)
			assert.ErrorIs(t, err, context.Canceled)
			_, err = p.readPage(t.Context(), uint64(len(want)/pageSize))
			assert.ErrorContains(t, err, "out of range")
		})
	}
}

func TestPageCompressionExperimentRejectsDamage(t *testing.T) {
	d, pageSize := pageExperimentFixture(t.Context(), t, 5)
	path := filepath.Join(t.TempDir(), "pages.experiment")
	require.NoError(t, buildPageExperiment(t.Context(), d.Path(), path, pageSize, 16))
	p, err := openPageExperiment(t.Context(), path)
	require.NoError(t, err)
	first := p.blocks[0]
	p.decoder.Close()
	require.NoError(t, p.file.Close())
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0}, int64(first.offset))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	p, err = openPageExperiment(t.Context(), path)
	require.NoError(t, err)
	_, err = p.readPage(t.Context(), 0)
	assert.Error(t, err)
	p.decoder.Close()
	require.NoError(t, p.file.Close())
	require.NoError(t, os.Truncate(path, int64(first.offset)))
	_, err = openPageExperiment(t.Context(), path)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestPageCompressionExperimentKeepsIncompressiblePage(t *testing.T) {
	rng := rand.New(rand.NewPCG(19, 31))
	want := make([]byte, 4096)
	for i := range want {
		want[i] = byte(rng.Uint64())
	}
	source := filepath.Join(t.TempDir(), "random.page")
	target := filepath.Join(t.TempDir(), "random.experiment")
	require.NoError(t, os.WriteFile(source, want, 0600))
	require.NoError(t, buildPageExperiment(t.Context(), source, target, 4096, 1))
	p, err := openPageExperiment(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { p.decoder.Close(); require.NoError(t, p.file.Close()) })
	assert.Zero(t, p.blocks[0].compressed, "unprofitable compression must keep original bytes")
	got, err := p.readPage(t.Context(), 0)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	info, err := p.file.Stat()
	require.NoError(t, err)
	assert.Equal(t, int64(4144), info.Size(), "account for header and index even without compression")
}

func BenchmarkSQLitePageCompression(b *testing.B) {
	d, pageSize := pageExperimentFixture(b.Context(), b, 10000)
	source, err := os.Open(d.Path())
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, source.Close()) })
	info, err := source.Stat()
	require.NoError(b, err)
	count := uint64(info.Size() / int64(pageSize))
	b.Run("plain", func(b *testing.B) {
		buf := make([]byte, pageSize)
		var n uint64
		for b.Loop() {
			_, err := source.ReadAt(buf, int64((n*7919)%count)*int64(pageSize))
			require.NoError(b, err)
			n++
		}
		b.ReportMetric(float64(info.Size()), "container-bytes")
	})
	for _, pages := range []int{1, 16, 256} {
		if pages*pageSize > 1<<20 {
			continue
		}
		path := filepath.Join(b.TempDir(), "pages.experiment")
		require.NoError(b, buildPageExperiment(b.Context(), d.Path(), path, pageSize, pages))
		p, err := openPageExperiment(b.Context(), path)
		require.NoError(b, err)
		b.Cleanup(func() { p.decoder.Close(); require.NoError(b, p.file.Close()) })
		compressed, err := p.file.Stat()
		require.NoError(b, err)
		// Verify a page from every block before timing.
		for i := uint64(0); i < count; i += uint64(pages) {
			want := make([]byte, pageSize)
			_, err := source.ReadAt(want, int64(i)*int64(pageSize))
			require.NoError(b, err)
			got, err := p.readPage(b.Context(), i)
			require.NoError(b, err)
			require.Equal(b, want, got)
		}
		b.Run(fmt.Sprintf("pages_%d", pages), func(b *testing.B) {
			var n uint64
			for b.Loop() {
				_, err := p.readPage(b.Context(), (n*7919)%count)
				require.NoError(b, err)
				n++
			}
			b.ReportMetric(float64(compressed.Size()), "container-bytes")
			b.ReportMetric(float64(pages), "decode-pages/read")
		})
	}
}
