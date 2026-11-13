package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func newDBPackDigestsCommand() *cobra.Command {
	var unpack bool
	cmd := &cobra.Command{
		Use:   "pack-digests",
		Short: "Pack canonical conversation digests without changing exports",
		Long: "Explicitly store canonical conversation digests as 32-byte values. Stop the daemon first. " +
			"Conversion commits bounded batches; interruption is safe and the command can be rerun. " +
			"Older builds refuse a packed archive. Use --unpack to restore TEXT before downgrading. " +
			"Run db compact separately to reclaim free file space.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadMinimalFor(config.ScopeLocal)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			result, err := runDBPackDigests(cmd.Context(), cfg, !unpack)
			if err != nil {
				return fmt.Errorf("converting conversation digests after %d changed rows (completed batches remain readable; rerun to finish): %w", result.ChangedRows, err)
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), result)
			}
			mode := "packed"
			if unpack {
				mode = "plain TEXT"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Conversation digests use %s; scanned %d rows, changed %d rows.\n", mode, result.ScannedRows, result.ChangedRows)
			fmt.Fprintln(cmd.OutOrStdout(), "File space is reclaimed separately with db compact.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&unpack, "unpack", false, "Restore exact TEXT digests and remove this format fence")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func runDBPackDigests(ctx context.Context, cfg config.Config, enabled bool) (db.DigestPackingResult, error) {
	// No daemon auto-start, replacement, or transport delegation: ownership is
	// required before opening the archive or changing its format.
	database, lock, err := openWriteDBWith(ctx, cfg, func(ctx context.Context, cfg config.Config) (*db.DB, error) {
		return db.OpenForStorageMaintenance(ctx, cfg.DBPath)
	})
	if err != nil {
		return db.DigestPackingResult{}, err
	}
	defer closeWriteDB(database, lock)
	return database.SetConversationDigestPacking(ctx, enabled)
}
