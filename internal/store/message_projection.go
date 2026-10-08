package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"

	"go.kenn.io/msgvault/internal/messageimport"
)

const projectionColumns = `rfc822_message_id,list_id,message_type,sent_at,received_at,read_at,delivered_at,internal_date,sender_id,is_from_me,source_is_from_me,identity_is_from_me,account_address,account_path,draft_authored,subject,reply_to_message_id,thread_position,is_read,is_delivered,is_sent,is_edited,is_forwarded,has_attachments,attachment_count`
const projectionAttachmentColumns = `filename,mime_type,size,content_hash,storage_path,media_type,width,height,duration_ms,thumbnail_hash,thumbnail_path,source_attachment_id,attachment_metadata,attachment_state,attachment_skip_reason,attachment_role,role_source,source_part_key,content_id,encryption_version`

type messageProjection struct {
	OriginalMessageID int64            `json:"original_message_id"`
	Headers           mail.Header      `json:"headers"`
	Message           map[string]any   `json:"message"`
	Recipients        []map[string]any `json:"recipients"`
	Labels            []map[string]any `json:"labels"`
	Attachments       []map[string]any `json:"attachments"`
}

type projectionQuerier struct {
	boundQuerier
	tx *loggedTx
}

func (q projectionQuerier) Query(query string, args ...any) (*loggedRows, error) {
	return q.tx.QueryContext(q.ctx, query, args...)
}

func projectionRows(q projectionQuerier, query string, id int64) ([]map[string]any, error) {
	rows, err := q.Query(query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		values, pointers := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, name := range columns {
			if b, ok := values[i].([]byte); ok {
				row[name] = string(b)
			} else {
				row[name] = values[i]
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func loadMessageProjection(q projectionQuerier, in messageimport.ImportMessage, destination int64) (*messageProjection, error) {
	if in.OriginalMessageID == 0 {
		return nil, nil
	}
	var originalID, threadID string
	var sourceID int64
	if err := q.QueryRow(`SELECT m.source_id,m.source_message_id,c.source_conversation_id FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=? AND m.message_type='email' AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`, in.OriginalMessageID).Scan(&sourceID, &originalID, &threadID); err != nil {
		return nil, fmt.Errorf("%w: original email unavailable", messageimport.ErrValidation)
	}
	if sourceID == destination || originalID != in.SourceMessageID || threadID != in.SourceConversationID {
		return nil, fmt.Errorf("%w: projection must preserve original message and conversation identities", messageimport.ErrValidation)
	}
	p := &messageProjection{OriginalMessageID: in.OriginalMessageID}
	rows, err := projectionRows(q, `SELECT source_id,source_message_id,metadata,`+projectionColumns+` FROM messages WHERE id=?`, in.OriginalMessageID)
	if err != nil {
		return nil, err
	}
	p.Message = rows[0]
	if p.Recipients, err = projectionRows(q, `SELECT participant_id,recipient_type,display_name,email_address FROM message_recipients WHERE message_id=? ORDER BY recipient_type,participant_id,id`, in.OriginalMessageID); err != nil {
		return nil, err
	}
	if p.Labels, err = projectionRows(q, `SELECT l.source_label_id,l.name,l.label_type,l.system_role,l.color FROM labels l JOIN message_labels ml ON ml.label_id=l.id WHERE ml.message_id=? ORDER BY l.source_label_id`, in.OriginalMessageID); err != nil {
		return nil, err
	}
	if p.Attachments, err = projectionRows(q, `SELECT `+projectionAttachmentColumns+` FROM attachments WHERE message_id=? ORDER BY id`, in.OriginalMessageID); err != nil {
		return nil, err
	}
	var raw []byte
	var compression sql.NullString
	var format string
	if err = q.QueryRow(`SELECT raw_data,compression,raw_format FROM message_raw WHERE message_id=?`, in.OriginalMessageID).Scan(&raw, &compression, &format); err != nil {
		return nil, err
	}
	if format != "mime" {
		return nil, fmt.Errorf("%w: original email requires MIME", messageimport.ErrValidation)
	}
	decoded, err := decodeMessageRaw(raw, compression)
	if err != nil {
		return nil, err
	}
	email, err := mail.ReadMessage(bytes.NewReader(decoded))
	if err != nil {
		return nil, fmt.Errorf("read original headers: %w", err)
	}
	p.Headers = email.Header
	return p, nil
}

func (s *Store) applyMessageProjection(q querier, id, sourceID int64, p *messageProjection, body string) error {
	if p == nil {
		return nil
	}
	if _, err := q.Exec(`UPDATE messages SET (`+projectionColumns+`)=(SELECT `+projectionColumns+` FROM messages WHERE id=?) WHERE id=?`, p.OriginalMessageID, id); err != nil {
		return err
	}
	if _, err := q.Exec(`UPDATE messages SET source_is_from_me=TRUE,is_from_me=TRUE,is_sent=TRUE WHERE id=? AND EXISTS(SELECT 1 FROM messages m JOIN sources s ON s.id=m.source_id JOIN message_labels ml ON ml.message_id=m.id JOIN labels l ON l.id=ml.label_id WHERE m.id=? AND s.source_type='gmail' AND l.source_label_id='SENT')`, id, p.OriginalMessageID); err != nil {
		return err
	}
	if _, err := q.Exec(`DELETE FROM message_recipients WHERE message_id=?`, id); err != nil {
		return err
	}
	if _, err := q.Exec(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name,email_address) SELECT ?,participant_id,recipient_type,display_name,email_address FROM message_recipients WHERE message_id=?`, id, p.OriginalMessageID); err != nil {
		return err
	}
	labels := make(map[string]LabelInfo, len(p.Labels))
	for _, label := range p.Labels {
		providerID, _ := label["source_label_id"].(string)
		name, _ := label["name"].(string)
		kind, _ := label["label_type"].(string)
		role, _ := label["system_role"].(string)
		labels[providerID] = LabelInfo{Name: name, Type: kind, SystemRole: role}
	}
	resolved, err := ensureLabelsBatchWith(q, sourceID, labels, nil)
	if err != nil {
		return err
	}
	for _, label := range p.Labels {
		providerID, _ := label["source_label_id"].(string)
		labelID := resolved[providerID]
		if _, err := q.Exec(`UPDATE labels SET color=? WHERE id=?`, label["color"], labelID); err != nil {
			return err
		}
		if _, err := q.Exec(`INSERT INTO message_labels(message_id,label_id) VALUES(?,?)`, id, labelID); err != nil {
			return err
		}
	}
	if _, err := q.Exec(`DELETE FROM attachments WHERE message_id=?`, id); err != nil {
		return err
	}
	if _, err := q.Exec(`INSERT INTO attachments(message_id,`+projectionAttachmentColumns+`) SELECT ?,`+projectionAttachmentColumns+` FROM attachments WHERE message_id=?`, id, p.OriginalMessageID); err != nil {
		return err
	}
	// Retain all original headers as evidence; do not manufacture signed MIME
	// containing transformed bytes under the original signature and encoding.
	raw, err := json.Marshal(map[string]any{"projection": p, "body_text": body})
	if err != nil {
		return err
	}
	if err := upsertMessageRawWithFormat(q, id, raw, "message-projection-json"); err != nil {
		return err
	}
	if s.fts5Available {
		return s.dialect.FTSUpsert(q, FTSDoc{MessageID: id, Subject: p.Headers.Get("Subject"), Body: body, FromAddr: p.Headers.Get("From"), ToAddrs: strings.Join(p.Headers["To"], ", "), CcAddrs: strings.Join(p.Headers["Cc"], ", ")})
	}
	return nil
}
