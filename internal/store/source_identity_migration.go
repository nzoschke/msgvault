package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrSourceMigrationConflict reports a migration whose mapping disagrees with
// the archive. Nothing was changed.
var ErrSourceMigrationConflict = errors.New("source migration identity conflict")

// ErrSourceMigrationValidation reports a malformed migration request.
var ErrSourceMigrationValidation = errors.New("invalid source migration")

// SourceMigrationMessage rekeys one archived message to its provider ID.
type SourceMigrationMessage struct {
	MessageID         int64
	SourceMessageID   string
	ProviderMessageID string
	Metadata          jsontext.Value
}

// SourceMigrationKept is an archived message that keeps its source message ID.
type SourceMigrationKept struct {
	MessageID       int64
	SourceMessageID string
}

// SourceIdentityMigration moves a source to another source type and rekeys its
// messages. Messages and Kept together must name every message row of the
// source exactly once.
type SourceIdentityMigration struct {
	ArchiveUID     string
	SourceID       int64
	Identifier     string
	FromSourceType string
	ToSourceType   string
	MetadataKey    string
	Messages       []SourceMigrationMessage
	Kept           []SourceMigrationKept
}

// SourceIdentityMigrationResult reports a migration. Messages counts every
// covered row; Changed counts rows this call rekeyed.
type SourceIdentityMigrationResult struct {
	Messages int
	Changed  int
}

// Validate checks the request without reading the archive.
func (in SourceIdentityMigration) Validate() error {
	valid := func(value string, limit int) bool {
		return strings.TrimSpace(value) != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
	}
	if !valid(in.ArchiveUID, 128) || in.SourceID <= 0 || !valid(in.Identifier, 512) ||
		!valid(in.FromSourceType, 80) || !valid(in.ToSourceType, 80) || in.FromSourceType == in.ToSourceType ||
		!valid(in.MetadataKey, 80) || len(in.Messages)+len(in.Kept) == 0 {
		return ErrSourceMigrationValidation
	}
	rows := map[int64]bool{}
	oldIDs := map[string]bool{}
	newIDs := map[string]bool{}
	for _, m := range in.Messages {
		if m.MessageID <= 0 || rows[m.MessageID] || !valid(m.SourceMessageID, 2048) || !valid(m.ProviderMessageID, 2048) ||
			m.SourceMessageID == m.ProviderMessageID || oldIDs[m.SourceMessageID] || newIDs[m.ProviderMessageID] {
			return fmt.Errorf("%w: message %d has a duplicate or ambiguous mapping", ErrSourceMigrationValidation, m.MessageID)
		}
		if m.Metadata.Kind() != '{' || !m.Metadata.IsValid() {
			return fmt.Errorf("%w: message %d metadata must be an object", ErrSourceMigrationValidation, m.MessageID)
		}
		var fields struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(m.Metadata, &fields); err != nil || fields.ID != m.ProviderMessageID {
			return fmt.Errorf("%w: message %d metadata id must equal provider_message_id", ErrSourceMigrationValidation, m.MessageID)
		}
		rows[m.MessageID], oldIDs[m.SourceMessageID], newIDs[m.ProviderMessageID] = true, true, true
	}
	for _, k := range in.Kept {
		if k.MessageID <= 0 || rows[k.MessageID] || !valid(k.SourceMessageID, 2048) || oldIDs[k.SourceMessageID] {
			return fmt.Errorf("%w: unmapped message %d is duplicated", ErrSourceMigrationValidation, k.MessageID)
		}
		rows[k.MessageID], oldIDs[k.SourceMessageID] = true, true
	}
	for id := range newIDs {
		if oldIDs[id] {
			return fmt.Errorf("%w: provider ID %q is also a source message ID", ErrSourceMigrationValidation, id)
		}
	}
	return nil
}

// MigrateSourceIdentityContext moves a source from FromSourceType to
// ToSourceType and rekeys each mapped message from its source message ID to
// its provider ID, storing Metadata under MetadataKey. Message IDs, the
// source ID, raw data, and labels stay. It runs in one transaction: any
// conflict changes nothing.
//
// A source that is already ToSourceType with every mapped message already
// rekeyed and every kept message unchanged is a successful no-op, so a
// repeated call with the same mapping succeeds.
func (s *Store) MigrateSourceIdentityContext(ctx context.Context, in SourceIdentityMigration) (SourceIdentityMigrationResult, error) {
	out := SourceIdentityMigrationResult{Messages: len(in.Messages) + len(in.Kept)}
	if err := in.Validate(); err != nil {
		return SourceIdentityMigrationResult{}, err
	}
	conflict := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrSourceMigrationConflict, fmt.Sprintf(format, args...))
	}
	err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{in.SourceID}}, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncSource(in.SourceID); err != nil {
			return err
		}
		var uid string
		if err := q.QueryRow(`SELECT value FROM archive_metadata WHERE key = 'archive_uid'`).Scan(&uid); err != nil {
			return fmt.Errorf("read archive identity: %w", err)
		}
		if uid != in.ArchiveUID {
			return conflict("archive_uid does not match this archive")
		}
		var sourceType, identifier string
		err := q.QueryRow(`SELECT source_type, identifier FROM sources WHERE id = ?`, in.SourceID).Scan(&sourceType, &identifier)
		if errors.Is(err, sql.ErrNoRows) {
			return conflict("source %d does not exist", in.SourceID)
		}
		if err != nil {
			return fmt.Errorf("read source: %w", err)
		}
		if identifier != in.Identifier {
			return conflict("source %d belongs to another account", in.SourceID)
		}
		migrated := sourceType == in.ToSourceType
		if !migrated && sourceType != in.FromSourceType {
			return conflict("source %d has type %q", in.SourceID, sourceType)
		}
		if !migrated {
			var other int64
			err := q.QueryRow(`SELECT id FROM sources WHERE source_type = ? AND identifier = ?`, in.ToSourceType, in.Identifier).Scan(&other)
			if err == nil {
				return conflict("a %s source for this account already exists (source %d)", in.ToSourceType, other)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("check existing %s source: %w", in.ToSourceType, err)
			}
		}

		stored := map[int64]string{}
		rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(source_message_id, '') FROM messages WHERE source_id = ?`, in.SourceID)
		if err != nil {
			return fmt.Errorf("list source messages: %w", err)
		}
		for rows.Next() {
			var id int64
			var sourceMessageID string
			if err := rows.Scan(&id, &sourceMessageID); err != nil {
				_ = rows.Close()
				return err
			}
			stored[id] = sourceMessageID
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		if len(stored) != len(in.Messages)+len(in.Kept) {
			return conflict("mapping covers %d messages but the source has %d", len(in.Messages)+len(in.Kept), len(stored))
		}
		for _, k := range in.Kept {
			current, ok := stored[k.MessageID]
			if !ok {
				return conflict("message %d is not in source %d", k.MessageID, in.SourceID)
			}
			if current != k.SourceMessageID {
				return conflict("message %d source message ID does not match", k.MessageID)
			}
		}
		for _, m := range in.Messages {
			current, ok := stored[m.MessageID]
			if !ok {
				return conflict("message %d is not in source %d", m.MessageID, in.SourceID)
			}
			want := m.SourceMessageID
			if migrated {
				want = m.ProviderMessageID
			}
			if current != want {
				return conflict("message %d source message ID does not match", m.MessageID)
			}
		}
		if migrated {
			// The migration metadata records provenance, not live Graph state.
			for _, m := range in.Messages {
				var metadata sql.NullString
				if err := q.QueryRow(`SELECT metadata FROM messages WHERE id = ?`, m.MessageID).Scan(&metadata); err != nil {
					return err
				}
				fields := map[string]jsontext.Value{}
				if metadata.Valid {
					if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
						return fmt.Errorf("decode message %d metadata: %w", m.MessageID, err)
					}
				}
				if _, ok := fields[in.MetadataKey]; !ok {
					return conflict("message %d has its provider ID without migration metadata", m.MessageID)
				}
			}
			return nil
		}

		if _, err := q.Exec(fmt.Sprintf(`UPDATE sources SET source_type = ?, updated_at = %s WHERE id = ? AND source_type = ?`, s.dialect.Now()),
			in.ToSourceType, in.SourceID, in.FromSourceType); err != nil {
			return fmt.Errorf("change source type: %w", err)
		}
		for _, m := range in.Messages {
			var metadata sql.NullString
			if err := q.QueryRow(`SELECT metadata FROM messages WHERE id = ?`, m.MessageID).Scan(&metadata); err != nil {
				return err
			}
			encoded, changed, err := mergeMetadataKey(metadata, in.MetadataKey, m.Metadata)
			if err != nil {
				return fmt.Errorf("message %d: %w", m.MessageID, err)
			}
			if !changed {
				encoded = metadata.String
			}
			result, err := q.Exec(fmt.Sprintf(`UPDATE messages SET source_message_id = ?, metadata = %s
				WHERE id = ? AND source_id = ? AND source_message_id = ?`, s.dialect.JSONBindExpr()),
				m.ProviderMessageID, sql.NullString{String: encoded, Valid: true}, m.MessageID, in.SourceID, m.SourceMessageID)
			if err != nil {
				return fmt.Errorf("rekey message %d: %w", m.MessageID, err)
			}
			if affected, err := result.RowsAffected(); err != nil {
				return err
			} else if affected != 1 {
				return conflict("message %d changed during migration", m.MessageID)
			}
			out.Changed++
		}
		return s.bumpDerivedDataRevisionContext(ctx, tx)
	})
	if err != nil {
		return SourceIdentityMigrationResult{}, err
	}
	return out, nil
}

func mergeMetadataKey(metadata sql.NullString, key string, value jsontext.Value) (string, bool, error) {
	fields := map[string]jsontext.Value{}
	if metadata.Valid && metadata.String != "" && metadata.String != "null" {
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return "", false, fmt.Errorf("decode message metadata: %w", err)
		}
		if fields == nil {
			fields = map[string]jsontext.Value{}
		}
	}
	canonical := value.Clone()
	if err := canonical.Canonicalize(); err != nil {
		return "", false, err
	}
	if old, ok := fields[key]; ok {
		oldCanonical := old.Clone()
		if oldCanonical.Canonicalize() == nil && string(oldCanonical) == string(canonical) {
			return "", false, nil
		}
	}
	fields[key] = canonical
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}
