package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/messageimport"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportMessagesEmailProjection(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(t, err)
	_, err = st.DB().Exec(`
 INSERT INTO participants(id,email_address,display_name) VALUES(101,'owner@example.com','Owner'),(102,'reader@example.com','Reader');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title) VALUES(101,?,'thread','email_thread','Original');
 INSERT INTO messages(id,source_id,conversation_id,source_message_id,message_type,sender_id,subject,sent_at,internal_date,rfc822_message_id,list_id,is_read,has_attachments,attachment_count) VALUES(101,?,101,'message','email',101,'Original','2026-10-01 12:00:00','2026-10-01 12:00:01','<original@example.com>','list.example.com',0,1,1);
 INSERT INTO message_bodies(message_id,body_text,body_html) VALUES(101,'Original quoted body','<p>Original</p>');
 INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name,email_address) VALUES(101,101,'from','Envelope Owner','owner@example.com'),(101,102,'to','Envelope Reader','reader@example.com');
 INSERT INTO labels(id,source_id,source_label_id,name,label_type,system_role,color) VALUES(101,?,'SENT','Sent','system','sent','red');
 INSERT INTO message_labels(message_id,label_id) VALUES(101,101);
 INSERT INTO attachments(message_id,filename,mime_type,size,content_hash,storage_path,source_attachment_id,attachment_role,role_source) VALUES(101,'proposal.pdf','application/pdf',123,'hash','ab/hash','part1','standalone','mime_disposition');
 `, source.ID, source.ID, source.ID)
	require.NoError(t, err)
	raw := []byte("From: Envelope Owner <owner@example.com>\r\nTo: Reader <reader@example.com>\r\nSubject: Original\r\nMessage-ID: <original@example.com>\r\nReply-To: replies@example.com\r\nX-Custom: first\r\nX-Custom: second\r\nContent-Type: text/plain\r\n\r\nOriginal quoted body")
	require.NoError(t, st.UpsertMessageRaw(101, raw))
	in := messageimport.ImportMessagesRequest{Source: messageimport.ImportSource{Type: "prepared-v1", Identifier: "prepared-v1:owner@example.com"}, Messages: []messageimport.ImportMessage{{OriginalMessageID: 101, SourceMessageID: "message", SourceConversationID: "thread", Subject: "Original", SentAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), BodyText: "Cleaned body", Metadata: map[string]any{"recipe": "v1"}}}}
	first, err := st.ImportMessages(t.Context(), in)
	require.NoError(t, err)
	id := first.Messages[0].MessageID
	var sender int64
	var subject, rfc, list, kind string
	var read, outgoing bool
	require.NoError(t, st.DB().QueryRow(`SELECT sender_id,subject,rfc822_message_id,list_id,message_type,is_read,is_from_me FROM messages WHERE id=?`, id).Scan(&sender, &subject, &rfc, &list, &kind, &read, &outgoing))
	assert.Equal(t, int64(101), sender)
	assert.Equal(t, "Original", subject)
	assert.Equal(t, "<original@example.com>", rfc)
	assert.Equal(t, "list.example.com", list)
	assert.Equal(t, "email", kind)
	assert.False(t, read)
	assert.True(t, outgoing)
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM message_recipients WHERE message_id=?`, id).Scan(&count))
	assert.Equal(t, 2, count)
	require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=? AND l.source_id=? AND l.source_label_id='SENT' AND l.color='red'`, id, first.SourceID).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM attachments WHERE message_id=? AND storage_path='ab/hash' AND attachment_role='standalone'`, id).Scan(&count))
	assert.Equal(t, 1, count)
	body, err := st.GetMessageBodyText(id)
	require.NoError(t, err)
	assert.Equal(t, "Cleaned body", body)
	metadata, err := st.GetMessageMetadata(id)
	require.NoError(t, err)
	var meta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(metadata.String), &meta))
	assert.Contains(t, string(meta[messageimport.ProjectionKey]), `"X-Custom":["first","second"]`)
	retry, err := st.ImportMessages(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", retry.Messages[0].Status)
	assert.Equal(t, id, retry.Messages[0].MessageID)
	in.Messages[0].BodyText = "Revised cleaned body"
	changed, err := st.ImportMessages(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, "updated", changed.Messages[0].Status)
	assert.Equal(t, id, changed.Messages[0].MessageID)
	_, err = st.DB().Exec(`UPDATE labels SET name='Renamed' WHERE id=101`)
	require.NoError(t, err)
	changed, err = st.ImportMessages(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, "updated", changed.Messages[0].Status)
	var label string
	require.NoError(t, st.DB().QueryRow(`SELECT name FROM labels WHERE source_id=? AND source_label_id='SENT'`, first.SourceID).Scan(&label))
	assert.Equal(t, "Renamed", label)
	originalBody, err := st.GetMessageBodyText(101)
	require.NoError(t, err)
	assert.Equal(t, "Original quoted body", originalBody)
	originalRaw, err := st.GetMessageRaw(101)
	require.NoError(t, err)
	assert.Equal(t, raw, originalRaw)

	projected := in
	projected.Source = messageimport.ImportSource{Type: "classified-v1", Identifier: "classified-v1:owner@example.com"}
	projected.Messages = append([]messageimport.ImportMessage(nil), in.Messages...)
	projected.Messages[0].OriginalMessageID = id
	copied, err := st.ImportMessages(t.Context(), projected)
	require.NoError(t, err)
	copiedID := copied.Messages[0].MessageID
	require.NoError(t, st.DB().QueryRow(`SELECT sender_id,is_from_me FROM messages WHERE id=?`, copiedID).Scan(&sender, &outgoing))
	assert.Equal(t, int64(101), sender)
	assert.True(t, outgoing)
	metadata, err = st.GetMessageMetadata(copiedID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(metadata.String), &meta))
	assert.Contains(t, string(meta[messageimport.ProjectionKey]), `"X-Custom":["first","second"]`)
	require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM attachments WHERE message_id=? AND storage_path='ab/hash'`, copiedID).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=? AND l.source_id=? AND l.name='Renamed'`, copiedID, copied.SourceID).Scan(&count))
	assert.Equal(t, 1, count)
	note := projected.Messages[0]
	note.OriginalMessageID = 0
	note.SourceMessageID = "decision:thread:hash"
	note.BodyText = "FYI"
	projected.Messages = []messageimport.ImportMessage{note}
	decision, err := st.ImportMessages(t.Context(), projected)
	require.NoError(t, err)
	var conversationID, decisionConversationID int64
	var conversationType string
	require.NoError(t, st.DB().QueryRow(`SELECT m.conversation_id,c.conversation_type FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=?`, copiedID).Scan(&conversationID, &conversationType))
	require.NoError(t, st.DB().QueryRow(`SELECT conversation_id FROM messages WHERE id=?`, decision.Messages[0].MessageID).Scan(&decisionConversationID))
	assert.Equal(t, conversationID, decisionConversationID)
	assert.Equal(t, "email_thread", conversationType)
	in.Messages[0].OriginalMessageID = 999
	_, err = st.ImportMessages(t.Context(), in)
	require.ErrorIs(t, err, messageimport.ErrValidation)
}
