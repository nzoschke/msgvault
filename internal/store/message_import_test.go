package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/messageimport"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportMessagesSnapshots(t *testing.T) {
	st := testutil.NewTestStore(t)
	in := messageimport.ImportMessagesRequest{
		Source:   messageimport.ImportSource{Type: "prepared-v1", Identifier: "prepared-v1:owner@example.com"},
		Messages: []messageimport.ImportMessage{{SourceMessageID: "thread:hash", SourceConversationID: "thread", Subject: "Planning", SentAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), BodyText: "The prepared conversation.", Metadata: map[string]any{"original_ids": []string{"a", "b"}}}},
	}
	first, err := st.ImportMessages(t.Context(), in)
	require.NoError(t, err)
	require.Len(t, first.Messages, 1)
	assert.Equal(t, "created", first.Messages[0].Status)
	body, err := st.GetMessageBodyText(first.Messages[0].MessageID)
	require.NoError(t, err)
	assert.Equal(t, in.Messages[0].BodyText, body)
	retry, err := st.ImportMessages(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, first.Messages[0].MessageID, retry.Messages[0].MessageID)
	assert.Equal(t, "unchanged", retry.Messages[0].Status)
	changed := in.Messages[0]
	changed.SourceMessageID = "other"
	in.Messages = append([]messageimport.ImportMessage{changed}, in.Messages...)
	in.Messages[1].BodyText = "Changed body under the old identity."
	_, err = st.ImportMessages(t.Context(), in)
	require.ErrorIs(t, err, messageimport.ErrConflict)
	var count int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM messages WHERE source_id = ?", first.SourceID).Scan(&count))
	assert.Equal(t, 1, count, "a conflict rolls back the entire batch")
	in.Messages = in.Messages[:1]
	in.Source.Identifier = "prepared-v1:other@example.com"
	second, err := st.ImportMessages(t.Context(), in)
	require.NoError(t, err)
	assert.NotEqual(t, first.SourceID, second.SourceID)
	assert.NotEqual(t, first.Messages[0].MessageID, second.Messages[0].MessageID)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.ImportMessages(ctx, in)
	require.Error(t, err)
}

func TestImportMessagesSourceIsolation(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("prepared-v1", "occupied")
	require.NoError(t, err)
	in := messageimport.ImportMessagesRequest{Source: messageimport.ImportSource{Type: "prepared-v1", Identifier: "occupied"}, Messages: []messageimport.ImportMessage{{SourceMessageID: "one", SourceConversationID: "one", Subject: "Test", SentAt: time.Now(), BodyText: "Body"}}}
	_, err = st.ImportMessages(t.Context(), in)
	require.ErrorIs(t, err, messageimport.ErrConflict)
	in.Source.Type = "gmail"
	_, err = st.ImportMessages(t.Context(), in)
	require.ErrorIs(t, err, messageimport.ErrValidation)
	in.Source.Type = "prepared-v1"
	in.Source.Identifier = "unused"
	in.Messages = append(in.Messages, in.Messages[0])
	_, err = st.ImportMessages(t.Context(), in)
	require.ErrorIs(t, err, messageimport.ErrValidation)
}
