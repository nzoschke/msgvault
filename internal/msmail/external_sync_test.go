package msmail

import (
	"bytes"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestExternalSyncUsesUpstreamCategories(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var refreshes atomic.Int32
	socket := credentialSocket(t, &refreshes)
	graph := newFakeGraph(t)
	graph.categories = map[string][]string{"m1": {"VIP", "Work"}}
	graph.put("m1", "inbox")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "alice@example.com", r.URL.Query().Get("account"))
		assert.Equal(t, "Bearer stale", r.Header.Get("Authorization"))
		req := r.Clone(r.Context())
		req.URL.Path = strings.TrimPrefix(r.URL.Path, "/proxy")
		response := httptest.NewRecorder()
		graph.serve(response, req)
		maps.Copy(w.Header(), response.Header())
		w.WriteHeader(response.Code)
		_, _ = w.Write(bytes.ReplaceAll(response.Body.Bytes(), []byte(graph.srv.URL), []byte(GraphBaseURL)))
	}))
	defer proxy.Close()
	client, err := NewExternalClient(ExternalIn{Account: "alice@example.com", Endpoint: proxy.URL + "/proxy", CredentialSocket: socket, QPS: 10000})
	requirements.NoError(err)
	archive := testutil.NewTestStore(t)
	opts := Options{Email: "alice@example.com", AttachmentsDir: t.TempDir()}
	first, err := Import(t.Context(), archive, client, opts, slog.Default())
	requirements.NoError(err)
	requirements.Positive(first.SyncRunID)
	ids, err := archive.MessageExistsBatch(first.SourceID, []string{"m1"})
	requirements.NoError(err)
	id := ids["m1"]
	requirements.Positive(id)
	for _, categories := range [][]string{{"VIP", "Work"}, {"FYI", "Work"}, {}} {
		graph.mu.Lock()
		graph.categories["m1"] = categories
		graph.mu.Unlock()
		graph.put("m1", "inbox")
		result, err := Import(t.Context(), archive, client, opts, slog.Default())
		requirements.NoError(err)
		assertions.Zero(result.Errors)
		message, err := archive.GetMessage(id)
		requirements.NoError(err)
		want := []string{"Inbox"}
		for _, category := range categories {
			want = append(want, "Category: "+category)
		}
		assertions.ElementsMatch(want, message.Labels)
	}
	assertions.Zero(refreshes.Load())
}
