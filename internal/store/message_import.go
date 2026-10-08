package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/batchmessageimport"
)

func (s *Store) ImportMessages(ctx context.Context, in batchmessageimport.ImportMessagesRequest) (batchmessageimport.ImportMessagesResponse, error) {
	if err := in.Validate(); err != nil {
		return batchmessageimport.ImportMessagesResponse{}, err
	}
	out := batchmessageimport.ImportMessagesResponse{Messages: []batchmessageimport.ImportMessageResult{}}
	err := s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		owner := `{"importer":"messages-v1"}`
		var config string
		err := q.QueryRow(`INSERT INTO sources (source_type, identifier, display_name, sync_config)
			VALUES (?, ?, ?, ?) ON CONFLICT (source_type, identifier) DO UPDATE
			SET identifier = sources.identifier RETURNING id, COALESCE(sync_config, '{}')`,
			in.Source.Type, in.Source.Identifier, in.Source.DisplayName, owner).Scan(&out.SourceID, &config)
		if err != nil {
			return fmt.Errorf("resolve import source: %w", err)
		}
		var state struct {
			Importer string `json:"importer"`
		}
		if json.Unmarshal([]byte(config), &state) != nil || state.Importer != batchmessageimport.Owner {
			return batchmessageimport.ErrConflict
		}
		if _, err := q.Exec(s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO collection_sources (collection_id, source_id)
			SELECT id, ? FROM collections WHERE name = ?`), out.SourceID, DefaultCollectionName); err != nil {
			return err
		}
		for _, m := range in.Messages {
			m.SentAt = m.SentAt.UTC()
			if len(m.Metadata) == 0 {
				m.Metadata = nil
			}
			raw, err := json.Marshal(m)
			if err != nil {
				return err
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(raw))
			var id int64
			var metadata string
			var deleted, sourceDeleted sql.NullTime
			err = q.QueryRow(`SELECT id, COALESCE(metadata, '{}'), deleted_at, deleted_from_source_at FROM messages
				WHERE source_id = ? AND source_message_id = ?`, out.SourceID, m.SourceMessageID).Scan(&id, &metadata, &deleted, &sourceDeleted)
			if err == nil {
				var prior map[string]json.RawMessage
				var hash string
				if json.Unmarshal([]byte(metadata), &prior) != nil || json.Unmarshal(prior[batchmessageimport.MetadataKey], &hash) != nil || hash != digest || deleted.Valid || sourceDeleted.Valid {
					return batchmessageimport.ErrConflict
				}
				out.Messages = append(out.Messages, batchmessageimport.ImportMessageResult{SourceMessageID: m.SourceMessageID, MessageID: id, Status: "unchanged"})
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			meta := map[string]any{batchmessageimport.MetadataKey: digest}
			for key, value := range m.Metadata {
				meta[key] = value
			}
			encoded, err := json.Marshal(meta)
			if err != nil {
				return err
			}
			storedMetadata := sql.NullString{String: string(encoded), Valid: true}
			snippet := []rune(m.BodyText)
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			id, err = s.persistMessageWithParticipantsTx(ctx, tx, nil, nil, func([]int64) *MessagePersistData {
				return &MessagePersistData{
					Message:      &Message{SourceID: out.SourceID, SourceMessageID: m.SourceMessageID, MessageType: "document", Subject: sql.NullString{String: m.Subject, Valid: true}, SentAt: sql.NullTime{Time: m.SentAt, Valid: true}, Snippet: sql.NullString{String: string(snippet), Valid: true}, SizeEstimate: int64(len(m.BodyText))},
					Conversation: &ConversationPersistData{SourceConversationID: m.SourceConversationID, ConversationType: "document", Title: m.Subject},
					BodyText:     sql.NullString{String: m.BodyText, Valid: true}, Metadata: &storedMetadata,
					RawMIME: raw, RawFormat: "message-import-json", FTS: &FTSDoc{Subject: m.Subject, Body: m.BodyText},
				}
			}, nil, nil)
			if err != nil {
				return err
			}
			out.Messages = append(out.Messages, batchmessageimport.ImportMessageResult{SourceMessageID: m.SourceMessageID, MessageID: id, Status: "created"})
		}
		return nil
	})
	if err != nil {
		return batchmessageimport.ImportMessagesResponse{}, err
	}
	return out, nil
}
