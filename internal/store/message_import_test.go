package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/batchmessageimport"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportMessagesSnapshots(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	in := batchmessageimport.ImportMessagesRequest{
		Source:   batchmessageimport.ImportSource{Type: "prepared-v1", Identifier: "prepared-v1:owner@example.com"},
		Messages: []batchmessageimport.ImportMessage{{SourceMessageID: "thread:hash", SourceConversationID: "thread", Subject: "Planning", SentAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), BodyText: "The prepared conversation.", Metadata: map[string]any{"original_ids": []string{"a", "b"}}}},
	}
	first, err := st.ImportMessages(t.Context(), in)
	requirements.NoError(err)
	requirements.Len(first.Messages, 1)
	assertions.Equal("created", first.Messages[0].Status)
	body, err := st.GetMessageBodyText(t.Context(), first.Messages[0].MessageID)
	requirements.NoError(err)
	assertions.Equal(in.Messages[0].BodyText, body)
	retry, err := st.ImportMessages(t.Context(), in)
	requirements.NoError(err)
	assertions.Equal(first.Messages[0].MessageID, retry.Messages[0].MessageID)
	assertions.Equal("unchanged", retry.Messages[0].Status)
	changed := in.Messages[0]
	changed.SourceMessageID = "other"
	in.Messages = append([]batchmessageimport.ImportMessage{changed}, in.Messages...)
	in.Messages[1].BodyText = "Changed body under the old identity."
	_, err = st.ImportMessages(t.Context(), in)
	requirements.ErrorIs(err, batchmessageimport.ErrConflict)
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT count(*) FROM messages WHERE source_id = ?"), first.SourceID).Scan(&count))
	assertions.Equal(1, count, "a conflict rolls back the entire batch")
	in.Messages = in.Messages[:1]
	in.Source.Identifier = "prepared-v1:other@example.com"
	second, err := st.ImportMessages(t.Context(), in)
	requirements.NoError(err)
	assertions.NotEqual(first.SourceID, second.SourceID)
	assertions.NotEqual(first.Messages[0].MessageID, second.Messages[0].MessageID)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.ImportMessages(ctx, in)
	requirements.Error(err)
}

func TestImportMessagesSourceIsolation(t *testing.T) {
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("prepared-v1", "occupied")
	requirements.NoError(err)
	in := batchmessageimport.ImportMessagesRequest{Source: batchmessageimport.ImportSource{Type: "prepared-v1", Identifier: "occupied"}, Messages: []batchmessageimport.ImportMessage{{SourceMessageID: "one", SourceConversationID: "one", Subject: "Test", SentAt: time.Now(), BodyText: "Body"}}}
	_, err = st.ImportMessages(t.Context(), in)
	requirements.ErrorIs(err, batchmessageimport.ErrConflict)
	in.Source.Type = "gmail"
	_, err = st.ImportMessages(t.Context(), in)
	requirements.ErrorIs(err, batchmessageimport.ErrValidation)
	in.Source.Type = "prepared-v1"
	in.Source.Identifier = "unused"
	in.Messages = append(in.Messages, in.Messages[0])
	_, err = st.ImportMessages(t.Context(), in)
	requirements.ErrorIs(err, batchmessageimport.ErrValidation)
}
