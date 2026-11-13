package db

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkConversationDigestPacking measures the complete archive file for a
// synthetic 10,000-row conversation index and a revision-indexed logical read.
// It includes both digest columns, retained tombstones, and exact legacy TEXT.
// It is not a whole-archive saving prediction or a migration throughput test.
func BenchmarkConversationDigestPacking(b *testing.B) {
	for _, packed := range []bool{false, true} {
		b.Run(fmt.Sprintf("packed_%t", packed), func(b *testing.B) {
			d := testDB(b)
			ctx := b.Context()
			require.NoError(b, d.Update(ctx, func(tx *sql.Tx) error {
				stmt, err := tx.PrepareContext(ctx, `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,digest,visible_digest,removed,deleted,revision) VALUES(?, ?, ?, 'assistant', ?, ?, ?, ?, ?)`)
				if err != nil {
					return err
				}
				defer stmt.Close()
				for i := range 10000 {
					digest, visible := helloDigest, abcDigest
					if i%10 == 0 {
						digest = "legacy:opaque"
						visible = ""
					}
					if _, err := stmt.ExecContext(ctx, fmt.Sprintf("session-%08d", i/100), fmt.Sprintf("opaque-%08d", i), i%100, digest, visible, i%100 < 95, i%100 < 95, i+1); err != nil {
						return err
					}
				}
				return nil
			}))
			if packed {
				_, err := d.SetConversationDigestPacking(ctx, true)
				require.NoError(b, err)
			}
			_, err := d.getWriter().Exec(ctx, `VACUUM`)
			require.NoError(b, err)
			_, err = d.getWriter().Exec(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
			require.NoError(b, err)
			info, err := os.Stat(d.Path())
			require.NoError(b, err)

			conn, err := d.getReader().Conn(ctx)
			require.NoError(b, err)
			defer conn.Close()
			stmt, err := conn.PrepareContext(ctx, `SELECT `+conversationChangeColumns+` FROM conversation_messages WHERE revision>? ORDER BY revision LIMIT 50`)
			require.NoError(b, err)
			defer stmt.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, err := stmt.QueryContext(ctx, (i*50)%9950)
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
			}
			b.ReportMetric(float64(info.Size()), "archive-bytes")
		})
	}
}

// BenchmarkConversationDigestNoop includes a real message reconciliation and
// transaction, then verifies that repeated identical imports publish nothing.
func BenchmarkConversationDigestNoop(b *testing.B) {
	for _, packed := range []bool{false, true} {
		b.Run(fmt.Sprintf("packed_%t", packed), func(b *testing.B) {
			d := testDB(b)
			ctx := b.Context()
			require.NoError(b, d.UpsertSession(ctx, Session{ID: "chat", Project: "sample", Machine: "local", Agent: "claude"}))
			msgs := []Message{{SessionID: "chat", Role: "assistant", Content: "hello", DialogueText: new("abc"), SourceUUID: "native"}}
			require.NoError(b, d.InsertMessages(ctx, msgs))
			before, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(b, err)
			if packed {
				_, err = d.SetConversationDigestPacking(ctx, true)
				require.NoError(b, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				require.NoError(b, d.ReplaceSessionMessages(ctx, "chat", msgs))
			}
			b.StopTimer()
			after, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
			require.NoError(b, err)
			require.Equal(b, before, after)
		})
	}
}
