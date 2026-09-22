package gmail

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExternalClient(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Chdir(t.TempDir())
	socket := "token.sock"
	listener, err := net.Listen("unix", socket)
	require.NoError(err)
	var refreshes atomic.Int32
	credentials := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("alice@example.com", r.URL.Query().Get("account"))
		token := "expired"
		if r.URL.Query().Get("refresh") == "true" {
			token = "fresh"
			refreshes.Add(1)
		}
		assert.NoError(json.NewEncoder(w).Encode(map[string]string{"access_token": token}))
	})}
	go func() { _ = credentials.Serve(listener) }()
	t.Cleanup(func() { _ = credentials.Close() })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/vault/v1/users/me/messages", r.URL.Path)
		assert.Equal("alice@example.com", r.URL.Query().Get("account"))
		assert.Equal("true", r.URL.Query().Get("includeSpamTrash"))
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"abc","threadId":"def"}],"nextPageToken":"page2"}`))
	}))
	defer upstream.Close()
	client, err := NewExternalClient(ExternalIn{Account: "alice@example.com", CredentialSocket: socket, Endpoint: upstream.URL + "/vault/v1", IncludeSpamTrash: true})
	require.NoError(err)
	page, err := client.ListMessages(t.Context(), "", "")
	require.NoError(err)
	assert.Equal("page2", page.NextPageToken)
	assert.Equal([]MessageID{{ID: "abc", ThreadID: "def"}}, page.Messages)
	assert.Equal(int32(1), refreshes.Load())
	require.Error(client.DeleteMessage(t.Context(), "abc"))
}

func TestExternalClientEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/v1", "https://user:secret@example.com/v1", "https://example.com/v1?token=secret", "/relative"} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := NewExternalClient(ExternalIn{Account: "alice@example.com", CredentialSocket: "/tmp/socket", Endpoint: endpoint})
			require.Error(t, err)
		})
	}
}
