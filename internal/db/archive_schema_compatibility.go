package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ArchiveSchemaTooNewError identifies a newer archive layout even when an
// unfinished parser upgrade has left its user_version at an older value.
type ArchiveSchemaTooNewError struct {
	Layout string
}

func (e *ArchiveSchemaTooNewError) Error() string {
	return fmt.Sprintf("archive schema %q requires a newer compatible AgentsView build; this build cannot safely open or copy it", e.Layout)
}

type archiveSchemaQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// validateArchiveSchemaCompatibility reads schema metadata only. Parser
// user_version is not a schema version: a newer build can install additive
// schema before a preservation rebuild finishes and advances that value.
// Keep these refusals until the corresponding readers, writers and copies
// preserve the newer state; raising dataVersion alone is not sufficient.
func validateArchiveSchemaCompatibility(ctx context.Context, q archiveSchemaQuerier, schema string) error {
	// Attachment names are internal constants, never archive or user data.
	switch schema {
	case "main", "old_db", "identity_source":
	default:
		return fmt.Errorf("unsupported archive schema attachment %q", schema)
	}
	var layout string
	err := q.QueryRowContext(ctx, `SELECT layout FROM (
        SELECT 'conversation_messages.' || name AS layout
        FROM pragma_table_xinfo('conversation_messages', ?)
        WHERE name COLLATE NOCASE IN ('usage_key', 'usage_revision')
        UNION ALL
        SELECT 'conversation_message_refs.' || name
        FROM pragma_table_xinfo('conversation_message_refs', ?)
        WHERE name COLLATE NOCASE IN ('usage_key', 'usage_revision')
        UNION ALL
        SELECT name FROM `+schema+`.sqlite_master
        WHERE type IN ('table', 'view') AND name COLLATE NOCASE IN (
            'session_usage_totals', 'diagnostic_events',
            'image_text_cache', 'image_text_occurrences',
            'session_frustration_markers'
        )
    ) ORDER BY layout LIMIT 1`, schema, schema).Scan(&layout)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking archive schema compatibility: %w", err)
	}
	return &ArchiveSchemaTooNewError{Layout: layout}
}
