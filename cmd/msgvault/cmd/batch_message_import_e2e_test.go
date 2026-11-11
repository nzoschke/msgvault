package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/batchmessageimport"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestImportMessagesClientToArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-key"}},
		Store:  &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler),
		OperationGate: api.NewSerialOperationGate(),
	})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	client, err := apiclient.New(server.URL)
	requirements.NoError(err)
	in := generated.ImportMessagesRequest{
		Source:   generated.ImportSource{Type: "prepared-v1", Identifier: "prepared-v1:owner@example.com"},
		Messages: []generated.ImportMessage{{SourceMessageID: "thread:hash", SourceConversationID: "thread", Subject: "Test", SentAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), BodyText: "Prepared evidence.", Metadata: map[string]any{"recipe": "v1", "original_ids": []string{"a", "b"}}}},
	}
	options := &generated.ImportMessagesRequestOptions{Body: &in}
	unauthorized, err := client.ImportMessagesWithResponse(t.Context(), options)
	requirements.Error(err)
	requirements.NotNil(unauthorized)
	assertions.Equal(http.StatusUnauthorized, unauthorized.StatusCode)
	auth := func(_ context.Context, r *http.Request) error { r.Header.Set("X-Api-Key", "synthetic-key"); return nil }
	first, err := client.ImportMessages(t.Context(), options, auth)
	requirements.NoError(err)
	requirements.Len(first.Messages, 1)
	assertions.Equal("created", string(first.Messages[0].Status))
	metadata, err := st.GetMessageMetadata(first.Messages[0].MessageID)
	requirements.NoError(err)
	var importedMetadata struct {
		OriginalIDs []string `json:"original_ids"`
	}
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &importedMetadata))
	assertions.Equal([]string{"a", "b"}, importedMetadata.OriginalIDs)
	retry, err := client.ImportMessages(t.Context(), options, auth)
	requirements.NoError(err)
	assertions.Equal(first.Messages[0].MessageID, retry.Messages[0].MessageID)
	assertions.Equal("unchanged", string(retry.Messages[0].Status))
	in.Messages[0].BodyText = "Changed evidence"
	conflict, err := client.ImportMessagesWithResponse(t.Context(), options, auth)
	requirements.Error(err)
	requirements.NotNil(conflict)
	assertions.Equal(http.StatusConflict, conflict.StatusCode)
	for _, body := range []string{`{}`, `{"source":{"type":"gmail","identifier":"owner@example.com"},"messages":[]}`} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "synthetic-key")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		assertions.Equal(http.StatusUnprocessableEntity, response.Code)
	}
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"unknown member", `{"unknown":true}`, http.StatusBadRequest},
		{"trailing JSON", `{} {}`, http.StatusBadRequest},
		{"oversized body", `{}` + strings.Repeat(" ", batchmessageimport.MaxRequestBytes), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/messages", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", "synthetic-key")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, req)
			assert.Equal(t, tc.status, response.Code)
		})
	}
}
