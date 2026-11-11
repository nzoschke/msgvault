package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/batchmessageimport"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportMessagesEmailProjection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	override := ""
	if st.IsPostgreSQL() {
		override = " OVERRIDING SYSTEM VALUE"
	}
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participants(id,email_address,display_name)`+override+` VALUES(101,'owner@example.com','Owner'),(102,'reader@example.com','Reader')`))
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title)`+override+` VALUES(101,?,'thread','email_thread','Original')`), source.ID)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO messages(id,source_id,conversation_id,source_message_id,message_type,sender_id,subject,sent_at,internal_date,rfc822_message_id,list_id,is_read,has_attachments,attachment_count)`+override+` VALUES(101,?,101,'message','email',101,'Original','2026-10-01 12:00:00','2026-10-01 12:00:01','<original@example.com>','list.example.com',FALSE,TRUE,1)`), source.ID)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO message_bodies(message_id,body_text,body_html) VALUES(101,'Original quoted body','<p>Original</p>')`))
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name,email_address) VALUES(101,101,'from','Envelope Owner','owner@example.com'),(101,102,'to','Envelope Reader','reader@example.com')`))
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO labels(id,source_id,source_label_id,name,label_type,system_role,color)`+override+` VALUES(101,?,'SENT','Sent','system','sent','red')`), source.ID)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO message_labels(message_id,label_id) VALUES(101,101)`))
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO attachments(message_id,filename,mime_type,size,content_hash,storage_path,source_attachment_id,attachment_role,role_source) VALUES(101,'proposal.pdf','application/pdf',123,'hash','ab/hash','part1','standalone','mime_disposition')`))
	requirements.NoError(err)
	raw := []byte("From: Envelope Owner <owner@example.com>\r\nTo: Reader <reader@example.com>\r\nSubject: Original\r\nMessage-ID: <original@example.com>\r\nReply-To: replies@example.com\r\nX-Custom: first\r\nX-Custom: second\r\nContent-Type: text/plain\r\n\r\nOriginal quoted body")
	requirements.NoError(st.UpsertMessageRaw(101, raw))
	in := batchmessageimport.ImportMessagesRequest{Source: batchmessageimport.ImportSource{Type: "prepared-v1", Identifier: "prepared-v1:owner@example.com"}, Messages: []batchmessageimport.ImportMessage{{OriginalMessageID: 101, SourceMessageID: "message", SourceConversationID: "thread", Subject: "Original", SentAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), BodyText: "Cleaned body", Metadata: map[string]any{"recipe": "v1"}}}}
	first, err := st.ImportMessages(t.Context(), in)
	requirements.NoError(err)
	id := first.Messages[0].MessageID
	var sender int64
	var subject, rfc, list, kind string
	var read, outgoing bool
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id,subject,rfc822_message_id,list_id,message_type,is_read,is_from_me FROM messages WHERE id=?`), id).Scan(&sender, &subject, &rfc, &list, &kind, &read, &outgoing))
	assertions.Equal(int64(101), sender)
	assertions.Equal("Original", subject)
	assertions.Equal("<original@example.com>", rfc)
	assertions.Equal("list.example.com", list)
	assertions.Equal("email", kind)
	assertions.False(read)
	assertions.True(outgoing)
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM message_recipients WHERE message_id=?`), id).Scan(&count))
	assertions.Equal(2, count)
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=? AND l.source_id=? AND l.source_label_id='SENT' AND l.color='red'`), id, first.SourceID).Scan(&count))
	assertions.Equal(1, count)
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM attachments WHERE message_id=? AND storage_path='ab/hash' AND attachment_role='standalone'`), id).Scan(&count))
	assertions.Equal(1, count)
	body, err := st.GetMessageBodyText(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("Cleaned body", body)
	metadata, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	var meta map[string]json.RawMessage
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &meta))
	var projection struct {
		Headers map[string][]string `json:"headers"`
	}
	requirements.NoError(json.Unmarshal(meta[batchmessageimport.ProjectionKey], &projection))
	assertions.Equal([]string{"first", "second"}, projection.Headers["X-Custom"])
	retry, err := st.ImportMessages(t.Context(), in)
	requirements.NoError(err)
	assertions.Equal("unchanged", retry.Messages[0].Status)
	assertions.Equal(id, retry.Messages[0].MessageID)
	in.Messages[0].BodyText = "Revised cleaned body"
	changed, err := st.ImportMessages(t.Context(), in)
	requirements.NoError(err)
	assertions.Equal("updated", changed.Messages[0].Status)
	assertions.Equal(id, changed.Messages[0].MessageID)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE labels SET name='Renamed' WHERE id=101`)
	requirements.NoError(err)
	changed, err = st.ImportMessages(t.Context(), in)
	requirements.NoError(err)
	assertions.Equal("updated", changed.Messages[0].Status)
	var label string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT name FROM labels WHERE source_id=? AND source_label_id='SENT'`), first.SourceID).Scan(&label))
	assertions.Equal("Renamed", label)
	originalBody, err := st.GetMessageBodyText(t.Context(), 101)
	requirements.NoError(err)
	assertions.Equal("Original quoted body", originalBody)
	originalRaw, err := st.GetMessageRaw(101)
	requirements.NoError(err)
	assertions.Equal(raw, originalRaw)

	projected := in
	projected.Source = batchmessageimport.ImportSource{Type: "classified-v1", Identifier: "classified-v1:owner@example.com"}
	projected.Messages = append([]batchmessageimport.ImportMessage(nil), in.Messages...)
	projected.Messages[0].OriginalMessageID = id
	copied, err := st.ImportMessages(t.Context(), projected)
	requirements.NoError(err)
	copiedID := copied.Messages[0].MessageID
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id,is_from_me FROM messages WHERE id=?`), copiedID).Scan(&sender, &outgoing))
	assertions.Equal(int64(101), sender)
	assertions.True(outgoing)
	metadata, err = st.GetMessageMetadata(copiedID)
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &meta))
	requirements.NoError(json.Unmarshal(meta[batchmessageimport.ProjectionKey], &projection))
	assertions.Equal([]string{"first", "second"}, projection.Headers["X-Custom"])
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM attachments WHERE message_id=? AND storage_path='ab/hash'`), copiedID).Scan(&count))
	assertions.Equal(1, count)
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=? AND l.source_id=? AND l.name='Renamed'`), copiedID, copied.SourceID).Scan(&count))
	assertions.Equal(1, count)
	note := projected.Messages[0]
	note.OriginalMessageID = 0
	note.SourceMessageID = "decision:thread:hash"
	note.BodyText = "FYI"
	projected.Messages = []batchmessageimport.ImportMessage{note}
	decision, err := st.ImportMessages(t.Context(), projected)
	requirements.NoError(err)
	var conversationID, decisionConversationID int64
	var conversationType string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT m.conversation_id,c.conversation_type FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=?`), copiedID).Scan(&conversationID, &conversationType))
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), decision.Messages[0].MessageID).Scan(&decisionConversationID))
	assertions.Equal(conversationID, decisionConversationID)
	assertions.Equal("email_thread", conversationType)
	in.Messages[0].OriginalMessageID = 999
	_, err = st.ImportMessages(t.Context(), in)
	requirements.ErrorIs(err, batchmessageimport.ErrValidation)
}
