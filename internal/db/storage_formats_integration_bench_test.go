package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const integrationStorageRows = 10_000
const integrationStorageHello = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
const integrationStorageABC = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

// The profile matches the standalone digest experiment: 100 sessions with 100
// retained rows each, 95% tombstones and 90% canonical digest pairs. Its 500
// live rows additionally have real archived messages so the same fixture can
// exercise unchanged application reconciliation, not a synthetic UPDATE.
func integrationStorageFixture(ctx context.Context, tb testing.TB) (*DB, []Message) {
	tb.Helper()
	d := testDB(tb)
	var all, target []Message
	for session := range 100 {
		id := fmt.Sprintf("session-%08d", session)
		require.NoError(tb, d.UpsertSession(ctx, Session{ID: id, Project: "sample", Machine: "local", Agent: "claude"}))
		for ordinal := range 5 {
			msg := Message{SessionID: id, Ordinal: ordinal, Role: "assistant", Content: "hello", DialogueText: new("abc"), SourceUUID: fmt.Sprintf("native-%08d", session*100+95+ordinal)}
			all = append(all, msg)
			if session == 50 {
				target = append(target, msg)
			}
		}
	}
	require.Len(tb, all, 500)
	require.Len(tb, target, 5)
	require.NoError(tb, d.InsertMessages(ctx, all))
	initialized, err := d.EnsureConversationExportInitialized(ctx)
	require.NoError(tb, err)
	require.True(tb, initialized)
	require.NoError(tb, d.Update(ctx, func(tx *sql.Tx) error {
		// Deterministic identities/revisions make every variant's logical input
		// exactly equal despite the normal initializer generating opaque IDs.
		live, err := tx.PrepareContext(ctx, `UPDATE conversation_messages SET message_id=?,revision=? WHERE session_id=? AND ordinal=? AND removed=0`)
		if err != nil {
			return err
		}
		defer live.Close()
		removed, err := tx.PrepareContext(ctx, `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,digest,text_bytes,visible_digest,visible_text_bytes,removed,deleted,revision) VALUES(?,?,?,'assistant',?,?,?,?,1,1,?)`)
		if err != nil {
			return err
		}
		defer removed.Close()
		for i := range integrationStorageRows {
			session, message := fmt.Sprintf("session-%08d", i/100), fmt.Sprintf("opaque-%08d", i)
			if i%100 >= 95 {
				result, err := live.ExecContext(ctx, message, i+1, session, i%100-95)
				if err != nil {
					return err
				}
				count, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if count != 1 {
					return fmt.Errorf("fixture live row %d matched %d rows", i, count)
				}
				continue
			}
			digest, visible, textBytes, visibleBytes := integrationStorageHello, integrationStorageABC, 5, 3
			if i%10 == 0 {
				digest, visible, textBytes, visibleBytes = "legacy:opaque", "", 0, 0
			}
			if _, err := removed.ExecContext(ctx, session, message, i%100, digest, textBytes, visible, visibleBytes, i+1); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES('conversation_publication_revision','10000') ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
		return err
	}))
	var total, tombstones, canonical, journal int
	require.NoError(tb, d.getReader().QueryRowContext(ctx, `SELECT COUNT(*),SUM(removed=1 AND deleted=1),SUM(digest=? AND visible_digest=?) FROM conversation_messages`, integrationStorageHello, integrationStorageABC).Scan(&total, &tombstones, &canonical))
	require.Equal(tb, integrationStorageRows, total)
	require.Equal(tb, 9500, tombstones)
	require.Equal(tb, 9000, canonical)
	require.NoError(tb, d.getReader().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_session_changes`).Scan(&journal))
	require.Zero(tb, journal)
	return d, target
}

type integrationStorageState struct {
	History, Journal, Identity          [sha256.Size]byte
	StoredExport, VisibleExport         [sha256.Size]byte
	StoredCheckpoint, VisibleCheckpoint string
}

// Hash every explicitly selected logical field in deterministic row order.
// Counts and independently fixed profile values protect against an empty or
// truncated query making two snapshots vacuously equal.
func integrationStorageHashRows(ctx context.Context, tb testing.TB, d *DB, query string, want int) [sha256.Size]byte {
	tb.Helper()
	rows, err := d.getReader().QueryContext(ctx, query)
	require.NoError(tb, err)
	defer rows.Close()
	columns, err := rows.Columns()
	require.NoError(tb, err)
	values, pointers := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	digest := sha256.New()
	count := 0
	for rows.Next() {
		require.NoError(tb, rows.Scan(pointers...))
		require.NoError(tb, json.MarshalWrite(digest, values))
		count++
	}
	require.NoError(tb, rows.Err())
	require.NoError(tb, rows.Close())
	require.Equal(tb, want, count)
	var result [sha256.Size]byte
	digest.Sum(result[:0])
	return result
}

func integrationStorageExportState(ctx context.Context, tb testing.TB, d *DB, mode string) ([sha256.Size]byte, string) {
	tb.Helper()
	digest := sha256.New()
	cursor, checkpoint := "", ""
	count, pages := 0, 0
	for {
		page, err := d.ExportConversationChanges(ctx, ConversationExportOptions{TextMode: mode, Cursor: cursor, Limit: 500})
		require.NoError(tb, err)
		require.NoError(tb, json.MarshalWrite(digest, page))
		count += len(page.Changes)
		pages++
		require.LessOrEqual(tb, pages, 20, "export must make progress through the complete fixture")
		if page.NextCursor == "" {
			checkpoint = page.Checkpoint
			break
		}
		require.NotEqual(tb, cursor, page.NextCursor)
		cursor = page.NextCursor
	}
	require.Equal(tb, integrationStorageRows, count)
	require.Equal(tb, 20, pages)
	require.NotEmpty(tb, checkpoint)
	var result [sha256.Size]byte
	digest.Sum(result[:0])
	return result, checkpoint
}

func integrationStorageSnapshot(ctx context.Context, tb testing.TB, d *DB) integrationStorageState {
	tb.Helper()
	var state integrationStorageState
	state.History = integrationStorageHashRows(ctx, tb, d, `SELECT session_id,message_id,ordinal,role,timestamp,source_id,body,`+conversationDigestReadSQL("digest", "archive_metadata")+`,text_bytes,`+conversationDigestReadSQL("visible_digest", "archive_metadata")+`,visible_text_bytes,visible_gap,gap,deleted,removed,revision FROM conversation_messages ORDER BY session_id,message_id`, integrationStorageRows)
	state.Journal = integrationStorageHashRows(ctx, tb, d, `SELECT session_id,revision,deleted,gap FROM conversation_session_changes ORDER BY session_id`, 0)
	state.Identity = integrationStorageHashRows(ctx, tb, d, `SELECT key,value FROM archive_metadata WHERE key IN ('archive_id','archive_salt','database_id','conversation_export_initialized','conversation_publication_revision') ORDER BY key`, 5)
	state.StoredExport, state.StoredCheckpoint = integrationStorageExportState(ctx, tb, d, "")
	state.VisibleExport, state.VisibleCheckpoint = integrationStorageExportState(ctx, tb, d, "visible")
	return state
}

func integrationStorageConvert(ctx context.Context, tb testing.TB, d *DB, feature string, enabled bool) time.Duration {
	tb.Helper()
	start := time.Now()
	switch feature {
	case "refs":
		report, err := d.SetConversationSessionRefs(ctx, enabled)
		elapsed := time.Since(start)
		require.NoError(tb, err)
		require.True(tb, report.Changed)
		require.Equal(tb, enabled, report.Enabled)
		require.EqualValues(tb, integrationStorageRows, report.Rows)
		return elapsed
	case "digests":
		report, err := d.SetConversationDigestPacking(ctx, enabled)
		elapsed := time.Since(start)
		require.NoError(tb, err)
		require.Equal(tb, enabled, report.Enabled)
		require.EqualValues(tb, integrationStorageRows, report.ScannedRows)
		require.EqualValues(tb, 9000, report.ChangedRows)
		return elapsed
	default:
		tb.Fatalf("unknown benchmark feature %q", feature)
		return 0
	}
}

// Prove that a composition benchmark actually retains both representations;
// logical equivalence alone would also pass if the second conversion silently
// disabled the first feature.
func integrationStorageAssertFormats(ctx context.Context, tb testing.TB, d *DB, refs, digests bool) {
	tb.Helper()
	var refsMode, digestMode, kind string
	require.NoError(tb, d.getReader().QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM archive_metadata WHERE key='conversation_session_refs'),''),COALESCE((SELECT value FROM archive_metadata WHERE key='conversation_digest_packing'),'')`).Scan(&refsMode, &digestMode))
	require.NoError(tb, d.getReader().QueryRowContext(ctx, `SELECT type FROM sqlite_master WHERE name='conversation_messages'`).Scan(&kind))
	expectedVersion := dataVersion
	table := "conversation_messages"
	if refs {
		expectedVersion |= archiveFormatSessionRefs
		require.Equal(tb, "v1", refsMode)
		require.Equal(tb, "view", kind)
		table = "conversation_message_refs"
		var mappings int
		require.NoError(tb, d.getReader().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_session_refs`).Scan(&mappings))
		require.Equal(tb, 100, mappings)
	} else {
		require.Empty(tb, refsMode)
		require.Equal(tb, "table", kind)
	}
	wantPacked := 0
	if digests {
		expectedVersion |= archiveFormatDigestPacking
		wantPacked = 9000
		require.Equal(tb, "v1", digestMode)
	} else {
		require.Empty(tb, digestMode)
	}
	var packed, version int
	require.NoError(tb, d.getReader().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE typeof(digest)='blob' AND typeof(visible_digest)='blob'`).Scan(&packed))
	require.Equal(tb, wantPacked, packed)
	require.NoError(tb, d.getReader().QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version))
	require.Equal(tb, expectedVersion, version)
}

// BenchmarkStorageFormatsIntegration compares actual independent production
// formats and both composition orders. Size is the final compacted archive,
// including the empty application schema, indexes, live messages and mappings;
// reductions must be computed against plain, never summed from separate PRs.
// Setup conversion/reversal metrics are single observations per fixture, not
// migration throughput samples or peak temporary-space measurements. Repeated
// child benchmark samples may report the same setup observation.
func BenchmarkStorageFormatsIntegration(b *testing.B) {
	var fixtureHistory [sha256.Size]byte
	for _, variant := range []struct {
		name  string
		order []string
	}{
		{"plain", nil}, {"refs_only", []string{"refs"}}, {"digests_only", []string{"digests"}},
		{"refs_then_digests", []string{"refs", "digests"}}, {"digests_then_refs", []string{"digests", "refs"}},
	} {
		b.Run(variant.name, func(b *testing.B) {
			ctx := b.Context()
			d, target := integrationStorageFixture(ctx, b)
			before := integrationStorageSnapshot(ctx, b, d)
			active := map[string]bool{}
			integrationStorageAssertFormats(ctx, b, d, false, false)
			if variant.name == "plain" {
				fixtureHistory = before.History
			} else if fixtureHistory != ([sha256.Size]byte{}) {
				require.Equal(b, fixtureHistory, before.History, "every variant must begin with identical logical history")
			}
			var conversion, reversal time.Duration
			for _, feature := range variant.order {
				conversion += integrationStorageConvert(ctx, b, d, feature, true)
				active[feature] = true
				integrationStorageAssertFormats(ctx, b, d, active["refs"], active["digests"])
				require.Equal(b, before, integrationStorageSnapshot(ctx, b, d), "conversion must preserve fields, tombstones, identities, revisions and both export checkpoints")
			}
			for i := len(variant.order) - 1; i >= 0; i-- {
				reversal += integrationStorageConvert(ctx, b, d, variant.order[i], false)
				active[variant.order[i]] = false
				integrationStorageAssertFormats(ctx, b, d, active["refs"], active["digests"])
				require.Equal(b, before, integrationStorageSnapshot(ctx, b, d), "reversal must preserve the same logical archive")
			}
			for _, feature := range variant.order {
				integrationStorageConvert(ctx, b, d, feature, true)
				active[feature] = true
				integrationStorageAssertFormats(ctx, b, d, active["refs"], active["digests"])
			}
			require.Equal(b, before, integrationStorageSnapshot(ctx, b, d))
			require.NoError(b, d.CheckpointWALTruncate(ctx))
			_, err := d.getWriter().ExecContext(ctx, `VACUUM; ANALYZE`)
			require.NoError(b, err)
			require.NoError(b, d.CheckpointWALTruncate(ctx))
			info, err := os.Stat(d.Path())
			require.NoError(b, err)
			var pages, pageSize int64
			require.NoError(b, d.getReader().QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages))
			require.NoError(b, d.getReader().QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize))
			require.Equal(b, pages*pageSize, info.Size(), "reported file must contain all compacted main-file pages")
			require.Equal(b, before, integrationStorageSnapshot(ctx, b, d))
			report := func(b *testing.B) {
				b.ReportMetric(float64(info.Size()), "archive-bytes")
				b.ReportMetric(float64(conversion)/float64(time.Millisecond), "setup-convert-ms")
				b.ReportMetric(float64(reversal)/float64(time.Millisecond), "setup-reverse-ms")
			}
			b.Run("revision_page", func(b *testing.B) {
				conn, err := d.getReader().Conn(b.Context())
				require.NoError(b, err)
				defer conn.Close()
				stmt, err := conn.PrepareContext(b.Context(), `SELECT `+conversationChangeColumns+` FROM conversation_messages WHERE revision>? ORDER BY revision LIMIT 50`)
				require.NoError(b, err)
				defer stmt.Close()
				var iteration int
				b.ReportAllocs()
				for b.Loop() {
					rows, err := stmt.QueryContext(b.Context(), (iteration*50)%9950)
					require.NoError(b, err)
					count := 0
					for rows.Next() {
						var change ConversationChange
						require.NoError(b, scanConversationChange(rows, &change))
						count++
					}
					require.NoError(b, rows.Err())
					require.NoError(b, rows.Close())
					require.Equal(b, 50, count)
					iteration++
				}
				require.Equal(b, before, integrationStorageSnapshot(b.Context(), b, d))
				report(b)
			})
			b.Run("unchanged_reconciliation", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					require.NoError(b, d.ReplaceSessionMessages(b.Context(), target[0].SessionID, target))
				}
				require.Equal(b, before, integrationStorageSnapshot(b.Context(), b, d), "unchanged imports must preserve both checkpoints and every retained row")
				report(b)
			})
		})
	}
}
