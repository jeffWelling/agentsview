package db

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Run with -benchtime=1x -count=3. Both layouts use the same populated archive,
// full production schema/FTS, and VACUUM compaction. Setup and rollback-only
// query samples are outside the timed conversion. No transcript files are read.
func BenchmarkMessageIndexConsolidation(b *testing.B) {
	for range b.N {
		b.StopTimer()
		d := seedMessageIndexBenchmark(b)
		baseline, err := d.EstimateCompact(b.Context())
		require.NoError(b, err)
		beforeIndex, hasDBStat := messageIndexBenchmarkTreeBytes(b, d)
		oldTimes := messageIndexBenchmarkQueries(b, d)
		b.StartTimer()
		result, err := d.Compact(b.Context(), CompactOptions{StagingDir: b.TempDir(), ConsolidateMessageIndex: true})
		b.StopTimer()
		require.NoError(b, err)
		require.Equal(b, "consolidated", result.MessageIndexStatus)
		afterIndex, _ := messageIndexBenchmarkTreeBytes(b, d)
		newTimes := messageIndexBenchmarkQueries(b, d)
		b.ReportMetric(float64(baseline.DatabaseBytes), "old-db-bytes")
		b.ReportMetric(float64(result.After.DatabaseBytes), "new-db-bytes")
		b.ReportMetric(float64(baseline.DatabaseBytes-result.After.DatabaseBytes), "saved-bytes")
		if hasDBStat {
			b.ReportMetric(float64(beforeIndex), "old-order-index-bytes")
			b.ReportMetric(float64(afterIndex), "new-order-index-bytes")
		}
		for name, duration := range oldTimes {
			b.ReportMetric(float64(duration.Nanoseconds()), "old-"+name+"-ns/op")
		}
		for name, duration := range newTimes {
			b.ReportMetric(float64(duration.Nanoseconds()), "new-"+name+"-ns/op")
		}
		require.NoError(b, d.Close())
	}
}

// seedMessageIndexBenchmark creates only generated data and returns the compacted
// legacy archive, allowing controlled comparisons to retain a second copy.
func seedMessageIndexBenchmark(b testing.TB) *DB {
	b.Helper()
	const sessions, messages = 1000, 100
	d := legacyMessageIndexDB(b)
	require.NoError(b, d.Update(b.Context(), func(tx *sql.Tx) error {
		session, err := tx.PrepareContext(b.Context(), `INSERT INTO sessions(id,project) VALUES(?,'benchmark')`)
		if err != nil {
			return err
		}
		defer session.Close()
		message, err := tx.PrepareContext(b.Context(), `INSERT INTO messages(session_id,ordinal,role,content,thinking_text,dialogue_text,timestamp,source_uuid) VALUES(?,?,'assistant',?,'',NULL,'2026-01-01T00:00:00Z',?)`)
		if err != nil {
			return err
		}
		defer message.Close()
		for s := range sessions {
			id := fmt.Sprintf("session-opaque-%032x", s)
			if _, err := session.ExecContext(b.Context(), id); err != nil {
				return err
			}
			for ordinal := range messages {
				content := fmt.Sprintf("Retained message %d about heliotrope search and storage. Unicode 雪 and exact text remain available.", ordinal)
				if _, err := message.ExecContext(b.Context(), id, ordinal, content, fmt.Sprintf("message-%032x", s*messages+ordinal)); err != nil {
					return err
				}
			}
		}
		return nil
	}))
	require.NoError(b, d.setDataVersion(b.Context()))
	_, err := d.Compact(b.Context(), CompactOptions{StagingDir: b.TempDir()})
	require.NoError(b, err)
	return d
}

func messageIndexBenchmarkTreeBytes(b *testing.B, d *DB) (int64, bool) {
	b.Helper()
	var total int64
	err := d.Reader().QueryRow(b.Context(), `SELECT COALESCE(sum(pgsize),0) FROM dbstat WHERE name='idx_messages_session_ordinal' OR name='sqlite_autoindex_messages_1'`).Scan(&total)
	if err != nil {
		b.Logf("per-index accounting unavailable: %v (enable SQLITE_ENABLE_DBSTAT_VTAB to include it)", err)
		return 0, false
	}
	return total, true
}

func messageIndexBenchmarkQueries(b *testing.B, d *DB) map[string]time.Duration {
	b.Helper()
	const samples = 250
	id := "session-opaque-00000000000000000000000000000000"
	results := map[string]time.Duration{}
	queries := map[string]string{
		"page": `SELECT id,content FROM messages INDEXED BY idx_messages_session_ordinal WHERE session_id=? AND ordinal>=90 ORDER BY ordinal LIMIT 10`,
		"fts":  `SELECT rowid FROM messages_fts WHERE messages_fts MATCH 'heliotrope' LIMIT 10`,
	}
	for name, query := range queries {
		args := []any{}
		if name == "page" {
			args = append(args, id)
		}
		run := func() {
			rows, err := d.rawReader().QueryContext(b.Context(), query, args...)
			require.NoError(b, err)
			for rows.Next() {
			}
			require.NoError(b, rows.Err())
			require.NoError(b, rows.Close())
		}
		// Check literal fixture results outside the timed loop.
		checked, err := d.rawReader().QueryContext(b.Context(), query, args...)
		require.NoError(b, err)
		count := 0
		for checked.Next() {
			var got int64
			if name == "page" {
				var content string
				require.NoError(b, checked.Scan(&got, &content))
				require.Equal(b, int64(91+count), got)
				require.Equal(b, fmt.Sprintf("Retained message %d about heliotrope search and storage. Unicode 雪 and exact text remain available.", 90+count), content)
			} else {
				require.NoError(b, checked.Scan(&got))
				require.Equal(b, int64(1+count), got)
			}
			count++
		}
		require.NoError(b, checked.Err())
		require.NoError(b, checked.Close())
		require.Equal(b, 10, count)
		for range 10 {
			run()
		}
		start := time.Now()
		for range samples {
			run()
		}
		results[name] = time.Since(start) / samples
	}
	for _, name := range []string{"append", "upsert", "noop"} {
		require.ErrorIs(b, d.Update(b.Context(), func(tx *sql.Tx) error {
			query := `INSERT INTO messages(session_id,ordinal,role,content) VALUES(?,?,'assistant',?)`
			if name != "append" {
				query += ` ON CONFLICT(session_id,ordinal) DO UPDATE SET content=excluded.content`
			}
			stmt, err := tx.PrepareContext(b.Context(), query)
			if err != nil {
				return err
			}
			defer stmt.Close()
			start := time.Now()
			for i := range samples {
				ordinal := 100 + i
				content := "new heliconia text"
				if name != "append" {
					ordinal = i % 100
				}
				if name == "upsert" {
					content = fmt.Sprintf("new heliconia text pass %d", i/100)
				}
				if name == "noop" {
					content = fmt.Sprintf("Retained message %d about heliotrope search and storage. Unicode 雪 and exact text remain available.", ordinal)
				}
				if _, err := stmt.ExecContext(b.Context(), id, ordinal, content); err != nil {
					return err
				}
			}
			results[name] = time.Since(start) / samples
			// Roll back so each sample layout retains identical rows and corpus.
			return errMessageIndexBenchmarkRollback
		}), errMessageIndexBenchmarkRollback)
	}
	return results
}

// Preserve the seeded corpus after each write sample.
var errMessageIndexBenchmarkRollback = errors.New("rollback benchmark sample")
