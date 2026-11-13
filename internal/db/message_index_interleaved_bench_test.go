package db

import (
	"database/sql"
	"encoding/json/v2"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// BenchmarkMessageIndexConsolidationInterleaved compares generated, equivalent
// archives in alternating ABBA/BAAB order. Run with -benchtime=1x. All reported
// latencies are batch means; this does not measure request p95 or physical I/O.
// AGENTSVIEW_INDEX_ABBA_REOPEN=both models restarting the application after the
// offline command; the default samples the object immediately after Compact.
// AGENTSVIEW_INDEX_BENCH_OUTPUT optionally records raw samples to a new file.
func BenchmarkMessageIndexConsolidationInterleaved(b *testing.B) {
	if b.N != 1 {
		b.Skip("run this experiment with -benchtime=1x")
	}
	b.StopTimer()
	fresh := seedMessageIndexBenchmark(b)
	require.NoError(b, fresh.setDataVersion(b.Context()))
	require.NoError(b, fresh.Update(b.Context(), func(tx *sql.Tx) error { _, err := tx.ExecContext(b.Context(), "ANALYZE"); return err }))
	seedPath := fresh.Path()
	require.NoError(b, fresh.Close())
	var err error
	fresh, err = OpenIsolated(b.Context(), seedPath)
	require.NoError(b, err)
	defer fresh.Close()
	result, err := fresh.Compact(b.Context(), CompactOptions{StagingDir: b.TempDir(), KeepBackup: true, ConsolidateMessageIndex: true})
	require.NoError(b, err)
	if os.Getenv("AGENTSVIEW_INDEX_ABBA_REOPEN") == "both" {
		canonicalPath := fresh.Path()
		require.NoError(b, fresh.Close())
		fresh, err = OpenIsolated(b.Context(), canonicalPath)
		require.NoError(b, err)
		defer fresh.Close()
	}
	old, err := OpenIsolated(b.Context(), result.BackupPath)
	require.NoError(b, err)
	defer old.Close()
	diagnostics := map[string]any{}
	for label, d := range map[string]*DB{"old": old, "new": fresh} {
		if info, err := os.Stat(d.Path() + "-wal"); err == nil {
			diagnostics[label+"_wal_before_checkpoint"] = info.Size()
		}
		require.NoError(b, d.CheckpointWALTruncate(b.Context()))
		if info, err := os.Stat(d.Path() + "-wal"); err == nil {
			diagnostics[label+"_wal_after_checkpoint"] = info.Size()
		}
	}
	for label, d := range map[string]*DB{"old": old, "new": fresh} {
		stats, err := d.Reader().QueryContext(b.Context(), `SELECT tbl,COALESCE(idx,''),stat FROM sqlite_stat1 ORDER BY tbl,idx`)
		require.NoError(b, err)
		var values [][]string
		for stats.Next() {
			var a, c, e string
			require.NoError(b, stats.Scan(&a, &c, &e))
			values = append(values, []string{a, c, e})
		}
		require.NoError(b, stats.Err())
		require.NoError(b, stats.Close())
		diagnostics[label+"_stat1"] = values
		for name, q := range map[string]string{"page": `EXPLAIN QUERY PLAN SELECT id,content FROM messages WHERE session_id='session-opaque-00000000000000000000000000000000' AND ordinal>=90 ORDER BY ordinal LIMIT 10`, "fts": `EXPLAIN QUERY PLAN SELECT rowid FROM messages_fts WHERE messages_fts MATCH 'heliotrope' LIMIT 10`} {
			rows, err := d.Reader().QueryContext(b.Context(), q)
			require.NoError(b, err)
			var plans []string
			for rows.Next() {
				var a, c, e int
				var detail string
				require.NoError(b, rows.Scan(&a, &c, &e, &detail))
				plans = append(plans, detail)
			}
			require.NoError(b, rows.Err())
			require.NoError(b, rows.Close())
			diagnostics[label+"_"+name+"_plan"] = plans
		}
	}
	for _, d := range []*DB{old, fresh} {
		var rows int
		require.NoError(b, d.Reader().QueryRow(b.Context(), `SELECT count(*) FROM messages`).Scan(&rows))
		require.Equal(b, 100000, rows)
		matches, err := d.Reader().QueryContext(b.Context(), `SELECT id,content FROM messages INDEXED BY idx_messages_session_ordinal WHERE session_id='session-opaque-00000000000000000000000000000000' AND ordinal>=90 ORDER BY ordinal LIMIT 10`)
		require.NoError(b, err)
		rows = 0
		for matches.Next() {
			var id int64
			var content string
			require.NoError(b, matches.Scan(&id, &content))
			require.Equal(b, int64(91+rows), id)
			require.Contains(b, content, "heliotrope search and storage")
			rows++
		}
		require.NoError(b, matches.Err())
		require.NoError(b, matches.Close())
		require.Equal(b, 10, rows)
	}
	type sample struct {
		Round       int              `json:"round"`
		Position    int              `json:"position"`
		Layout      string           `json:"layout"`
		Nanoseconds map[string]int64 `json:"nanoseconds"`
	}
	var samples []sample
	// Check that the FTS query being timed returns ten distinct matches.
	for _, d := range []*DB{old, fresh} {
		rows, err := d.Reader().QueryContext(b.Context(), `SELECT rowid FROM messages_fts WHERE messages_fts MATCH 'heliotrope' LIMIT 10`)
		require.NoError(b, err)
		seen := map[int64]bool{}
		for rows.Next() {
			var id int64
			require.NoError(b, rows.Scan(&id))
			require.False(b, seen[id])
			seen[id] = true
		}
		require.NoError(b, rows.Err())
		require.NoError(b, rows.Close())
		require.Len(b, seen, 10)
	}
	// Warm both archives with the same complete operation set before timing.
	messageIndexBenchmarkQueries(b, old)
	messageIndexBenchmarkQueries(b, fresh)
	for round := 0; round < 10; round++ {
		order := []*DB{old, fresh, fresh, old}
		if round%2 == 1 {
			order = []*DB{fresh, old, old, fresh}
		}
		for pos, d := range order {
			timing := messageIndexBenchmarkQueries(b, d)
			values := map[string]int64{}
			for name, duration := range timing {
				values[name] = duration.Nanoseconds()
			}
			label := "old"
			if d == fresh {
				label = "new"
			}
			samples = append(samples, sample{Round: round, Position: pos, Layout: label, Nanoseconds: values})
		}
	}
	output := os.Getenv("AGENTSVIEW_INDEX_BENCH_OUTPUT")
	data, err := json.Marshal(struct {
		Date        string         `json:"date"`
		Protocol    string         `json:"protocol"`
		Diagnostics map[string]any `json:"diagnostics"`
		Samples     []sample       `json:"samples"`
	}{time.Now().UTC().Format("2006-01-02"), "10 alternating ABBA/BAAB rounds; 20 batch means per layout; 250 operations per case per batch; warm reads; rollback-only writes", diagnostics, samples})
	require.NoError(b, err)
	if output != "" {
		file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		require.NoError(b, err)
		_, err = file.Write(data)
		require.NoError(b, err)
		require.NoError(b, file.Close())
	}
	for _, layout := range []string{"old", "new"} {
		for _, operation := range []string{"page", "fts", "append", "upsert", "noop"} {
			var values []int64
			for _, sample := range samples {
				if sample.Layout == layout {
					values = append(values, sample.Nanoseconds[operation])
				}
			}
			slices.Sort(values)
			median := (float64(values[len(values)/2-1]) + float64(values[len(values)/2])) / 2
			b.ReportMetric(median, layout+"-"+operation+"-ns/op")
		}
	}
	b.ReportMetric(float64(len(samples)), "batches")
}
