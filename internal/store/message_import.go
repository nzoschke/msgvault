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
			projection, err := loadMessageProjection(projectionQuerier{q, tx}, m, out.SourceID)
			if err != nil {
				return err
			}
			m.SentAt = m.SentAt.UTC()
			if len(m.Metadata) == 0 {
				m.Metadata = nil
			}
			raw, err := json.Marshal(struct {
				Message    batchmessageimport.ImportMessage
				Projection *messageProjection
			}{m, projection})
			if projection == nil {
				raw, err = json.Marshal(m)
			}
			if err != nil {
				return err
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(raw))
			var id int64
			var metadata string
			var deleted, sourceDeleted sql.NullTime
			status := "created"
			err = q.QueryRow(`SELECT id, COALESCE(metadata, '{}'), deleted_at, deleted_from_source_at FROM messages
				WHERE source_id = ? AND source_message_id = ?`, out.SourceID, m.SourceMessageID).Scan(&id, &metadata, &deleted, &sourceDeleted)
			if err == nil {
				var prior map[string]json.RawMessage
				var hash string
				if json.Unmarshal([]byte(metadata), &prior) != nil || json.Unmarshal(prior[batchmessageimport.MetadataKey], &hash) != nil || deleted.Valid || sourceDeleted.Valid {
					return batchmessageimport.ErrConflict
				}
				if hash == digest {
					out.Messages = append(out.Messages, batchmessageimport.ImportMessageResult{SourceMessageID: m.SourceMessageID, MessageID: id, Status: "unchanged"})
					continue
				}
				var previous messageProjection
				if projection == nil || json.Unmarshal(prior[batchmessageimport.ProjectionKey], &previous) != nil || previous.OriginalMessageID != projection.OriginalMessageID {
					return batchmessageimport.ErrConflict
				}
				status = "updated"
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			meta := map[string]any{batchmessageimport.MetadataKey: digest}
			for key, value := range m.Metadata {
				meta[key] = value
			}
			conversationType := "document"
			if projection != nil {
				meta[batchmessageimport.ProjectionKey] = projection
				conversationType = "email_thread"
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
					Conversation: &ConversationPersistData{SourceConversationID: m.SourceConversationID, ConversationType: conversationType, Title: m.Subject, PreserveExistingType: projection == nil},
					BodyText:     sql.NullString{String: m.BodyText, Valid: true}, Metadata: &storedMetadata,
					RawMIME: raw, RawFormat: "message-import-json", FTS: &FTSDoc{Subject: m.Subject, Body: m.BodyText},
				}
			}, nil, nil)
			if err != nil {
				return err
			}
			if err := s.applyMessageProjection(q, id, out.SourceID, projection, m.BodyText); err != nil {
				return err
			}
			out.Messages = append(out.Messages, batchmessageimport.ImportMessageResult{SourceMessageID: m.SourceMessageID, MessageID: id, Status: status})
		}
		return nil
	})
	if err != nil {
		return batchmessageimport.ImportMessagesResponse{}, err
	}
	return out, nil
}
