package cmd

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestImportMessagesClientToArchive(t *testing.T) {
	st := testutil.NewTestStore(t)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-key"}},
		Store:  &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler),
		OperationGate: api.NewSerialOperationGate(),
	})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	client, err := apiclient.New(server.URL)
	require.NoError(t, err)
	in := generated.ImportMessagesRequest{
		Source:   generated.ImportSource{Type: "prepared-v1", Identifier: "prepared-v1:owner@example.com"},
		Messages: []generated.ImportMessage{{SourceMessageID: "thread:hash", SourceConversationID: "thread", Subject: "Test", SentAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), BodyText: "Prepared evidence.", Metadata: map[string]any{"recipe": "v1", "original_ids": []string{"a", "b"}}}},
	}
	options := &generated.ImportMessagesRequestOptions{Body: &in}
	unauthorized, err := client.ImportMessagesWithResponse(t.Context(), options)
	require.Error(t, err)
	require.NotNil(t, unauthorized)
	assert.Equal(t, http.StatusUnauthorized, unauthorized.StatusCode)
	auth := func(_ context.Context, r *http.Request) error { r.Header.Set("X-Api-Key", "synthetic-key"); return nil }
	first, err := client.ImportMessages(t.Context(), options, auth)
	require.NoError(t, err)
	require.Len(t, first.Messages, 1)
	assert.Equal(t, "created", string(first.Messages[0].Status))
	metadata, err := st.GetMessageMetadata(first.Messages[0].MessageID)
	require.NoError(t, err)
	assert.Contains(t, metadata.String, `"original_ids":["a","b"]`)
	retry, err := client.ImportMessages(t.Context(), options, auth)
	require.NoError(t, err)
	assert.Equal(t, first.Messages[0].MessageID, retry.Messages[0].MessageID)
	assert.Equal(t, "unchanged", string(retry.Messages[0].Status))
	in.Messages[0].BodyText = "Changed evidence"
	conflict, err := client.ImportMessagesWithResponse(t.Context(), options, auth)
	require.Error(t, err)
	require.NotNil(t, conflict)
	assert.Equal(t, http.StatusConflict, conflict.StatusCode)
	for _, body := range []string{`{}`, `{"source":{"type":"gmail","identifier":"owner@example.com"},"messages":[]}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/import/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "synthetic-key")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
	}
}
