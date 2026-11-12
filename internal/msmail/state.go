package msmail

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
)

// saveMailState keeps provider context on the canonical row. Consumers can read
// it without maintaining a second mailbox inventory or downloading MIME again.
// Migration provenance remains separate and is never treated as observed state.
func (s *syncer) saveMailState(ctx context.Context, messageID, folderLabel int64, observed *DeltaMessage) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	var metadata sql.NullString
	if err = tx.QueryRowContext(ctx, s.st.Rebind(`SELECT source_message_id,metadata FROM messages WHERE id=? AND source_id=?`), messageID, s.sourceID).Scan(&id, &metadata); err != nil {
		return err
	}
	document := map[string]jsontext.Value{}
	if metadata.Valid && metadata.String != "" {
		if err = json.Unmarshal([]byte(metadata.String), &document); err != nil {
			return fmt.Errorf("decode mail metadata: %w", err)
		}
	}
	if document == nil {
		document = map[string]jsontext.Value{}
	}
	state := map[string]any{}
	if raw := document["_msgvault_msmail"]; len(raw) > 0 {
		if err = json.Unmarshal(raw, &state); err != nil {
			return err
		}
	}
	if state == nil {
		state = map[string]any{}
	}
	var folderID string
	if err = tx.QueryRowContext(ctx, s.st.Rebind(`SELECT source_label_id FROM labels WHERE id=? AND source_id=?`), folderLabel, s.sourceID).Scan(&folderID); err != nil {
		return err
	}
	state["id"] = id
	state["parentFolderId"] = folderID
	state["inInbox"] = s.inbox != "" && folderID == s.inbox
	state["isDraft"] = s.drafts != "" && folderID == s.drafts
	if observed != nil {
		if observed.ConversationID != "" {
			state["conversationId"] = observed.ConversationID
		}
		if observed.IsDraft != nil {
			state["isDraft"] = *observed.IsDraft
		}
		if !observed.ReceivedDateTime.IsZero() {
			state["receivedDateTime"] = observed.ReceivedDateTime.UTC().Format(time.RFC3339Nano)
		}
	}
	rows, err := tx.QueryContext(ctx, s.st.Rebind(`SELECT l.source_label_id FROM labels l JOIN message_labels ml ON ml.label_id=l.id WHERE ml.message_id=? AND l.source_id=? ORDER BY l.source_label_id`), messageID, s.sourceID)
	if err != nil {
		return err
	}
	categories := []string{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			_ = rows.Close()
			return err
		}
		if name, ok := strings.CutPrefix(key, "msmail-category:"); ok {
			categories = append(categories, name)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	state["categories"] = categories
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	document["_msgvault_msmail"] = raw
	data, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.st.Rebind(`UPDATE messages SET metadata=? WHERE id=? AND source_id=?`), string(data), messageID, s.sourceID); err != nil {
		return err
	}
	return tx.Commit()
}
