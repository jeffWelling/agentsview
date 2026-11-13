package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func newDBSessionRefsCommand() *cobra.Command {
	var restore, yes bool
	cmd := &cobra.Command{
		Use: "session-refs", Short: "Store conversation-history session keys once per session",
		Long: "Convert conversation history to permanent integer session references without changing exported IDs, text, revisions, or cursors. Stop the daemon and close other direct clients first. Keep a verified backup and this build available. Use --restore-text before returning to an ordinary build. Conversion stages a verified copy and needs extra disk space; run db compact afterward to reclaim freed pages.",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			jsonOutput := outputFormat(cmd) == "json"
			if jsonOutput && !yes {
				return errors.New("--format json requires --yes for db session-refs")
			}
			if !yes {
				prompt := "Convert conversation history to integer session references? A verified backup and this compatible build are required."
				if restore {
					prompt = "Restore ordinary text session keys in conversation history?"
				}
				if !confirm(cmd.InOrStdin(), cmd.ErrOrStderr(), prompt) {
					fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
					return nil
				}
			}
			cfg, err := config.LoadMinimalFor(config.ScopeLocal)
			if err != nil {
				return err
			}
			report, err := runDBSessionRefs(cmd.Context(), cfg, !restore)
			if err != nil {
				return fmt.Errorf("conversation format conversion did not complete: %w; keep using this build and repeat the command to restart staging", err)
			}

			if jsonOutput {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), report)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Conversation rows: %d. Integer session references enabled: %t. Representation changed: %t.\n", report.Rows, report.Enabled, report.Changed)
			return nil
		},
	}
	cmd.Flags().BoolVar(&restore, "restore-text", false, "Restore ordinary text session keys and remove the compatibility gate")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func runDBSessionRefs(ctx context.Context, cfg config.Config, enabled bool) (db.ConversationSessionRefsReport, error) {
	database, lock, err := openWriteDBWith(ctx, cfg, func(ctx context.Context, cfg config.Config) (*db.DB, error) {
		return db.OpenForStorageMaintenance(ctx, cfg.DBPath)
	})
	if err != nil {
		return db.ConversationSessionRefsReport{}, err
	}
	defer closeWriteDB(database, lock)
	return database.SetConversationSessionRefs(ctx, enabled)
}
