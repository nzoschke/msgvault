package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const SyncRunItemStatusSuccess = "success"

// RecordSyncRunMessages records archived members of a successfully handled page.
// Existing diagnostics win over success, and replaying a page is idempotent.
func (s *Store) RecordSyncRunMessages(ctx context.Context, syncID, sourceID int64, messageIDs []string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	if s.syncGeneration != nil && s.syncGeneration.runID != syncID {
		return ErrSyncRunSuperseded
	}
	return s.withSyncSourceWriteContext(ctx, sourceID, func(q querier) error {
		for start := 0; start < len(messageIDs); start += 500 {
			ids := messageIDs[start:min(start+500, len(messageIDs))]
			args := []any{syncID, SyncRunItemStatusSuccess, sourceID}
			for _, id := range ids {
				args = append(args, id)
			}
			args = append(args, syncID)
			_, err := q.Exec(`INSERT INTO sync_run_items
				(sync_run_id, source_message_id, phase, status, error_kind, error_message)
				SELECT ?, m.source_message_id, 'ingest', ?, '', '' FROM messages m
				WHERE m.source_id = ? AND m.source_message_id IN (`+placeholders(len(ids))+`)
				AND NOT EXISTS (SELECT 1 FROM sync_run_items i
					WHERE i.sync_run_id = ? AND i.source_message_id = m.source_message_id)`, args...)
			if err != nil {
				return fmt.Errorf("record sync messages: %w", err)
			}
		}
		return nil
	})
}

type SyncRunResult struct {
	ID              int64  `json:"id"`
	SourceMessageID string `json:"source_message_id"`
	MessageID       *int64 `json:"message_id,omitempty" nullable:"false"`
	Phase           string `json:"phase"`
	Status          string `json:"status"`
	ErrorKind       string `json:"error_kind,omitempty"`
	ErrorMessage    string `json:"error_message,omitempty"`
}

type SyncRunResults struct {
	SyncRunID   int64           `json:"sync_run_id"`
	SourceID    int64           `json:"source_id"`
	RunStatus   string          `json:"run_status"`
	Items       []SyncRunResult `json:"items"`
	NextAfterID int64           `json:"next_after_id"`
}

// ListSyncRunResults uses an ascending item ID cursor, including while a run is
// active. MessageID resolves the current archive row and is nil after removal.
func (s *Store) ListSyncRunResults(ctx context.Context, syncID, afterID int64, limit int) (*SyncRunResults, error) {
	if syncID <= 0 || afterID < 0 || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("invalid sync result pagination")
	}
	page := &SyncRunResults{SyncRunID: syncID, NextAfterID: afterID, Items: []SyncRunResult{}}
	err := s.db.QueryRowContext(ctx, `SELECT source_id, status FROM sync_runs WHERE id = ?`, syncID).Scan(&page.SourceID, &page.RunStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSyncRunNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get sync run: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.source_message_id, m.id,
		i.phase, i.status, i.error_kind, i.error_message
		FROM sync_run_items i LEFT JOIN messages m
		ON m.source_id = ? AND m.source_message_id = i.source_message_id
		WHERE i.sync_run_id = ? AND i.id > ? ORDER BY i.id LIMIT ?`, page.SourceID, syncID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list sync results: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item SyncRunResult
		if err := rows.Scan(&item.ID, &item.SourceMessageID, &item.MessageID, &item.Phase, &item.Status, &item.ErrorKind, &item.ErrorMessage); err != nil {
			return nil, fmt.Errorf("scan sync result: %w", err)
		}
		page.Items = append(page.Items, item)
		page.NextAfterID = item.ID
	}
	return page, rows.Err()
}
