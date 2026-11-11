package msmail

import (
	"context"
	"encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/msgraph"
)

// credentialSocket serves GET /token on a Unix socket. The token is "stale"
// until a refresh, then "fresh".
func credentialSocket(t *testing.T, refreshes *atomic.Int32) string {
	t.Helper()
	t.Chdir(t.TempDir())
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", "token.sock")
	require.NoError(t, err)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/token", r.URL.Path)
		assert.Equal(t, "alice@example.com", r.URL.Query().Get("account"))
		assert.Equal(t, "outlook", r.URL.Query().Get("provider"))
		token := "stale"
		if r.URL.Query().Get("refresh") == "true" {
			refreshes.Add(1)
			token = "fresh"
		}
		assert.NoError(t, json.MarshalWrite(w, map[string]string{"access_token": token}))
	})}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "token.sock"
}

func TestExternalClientProxiesMailReads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var refreshes atomic.Int32
	socket := credentialSocket(t, &refreshes)
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.EscapedPath())
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("alice@example.com", r.URL.Query().Get("account"))
		assert.Equal(`IdType="ImmutableId", odata.maxpagesize=1000`, r.Header.Get("Prefer"))
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.EscapedPath() {
		case "/api/outlook/vault/v1/me/mailFolders/inbox":
			_, _ = w.Write([]byte(`{"id":"in"}`))
		case "/api/outlook/vault/v1/me/mailFolders/A%2FB%3D/messages/delta":
			assert.Equal("tok1", r.URL.Query().Get("$deltatoken"))
			_, _ = w.Write([]byte(`{"value":[]}`))
		case "/api/outlook/vault/v1/me/messages/m1/$value":
			_, _ = w.Write([]byte("MIME"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	c, err := NewExternalClient(ExternalIn{Account: "alice@example.com", CredentialSocket: socket, Endpoint: upstream.URL + "/api/outlook/vault/v1"})
	require.NoError(err)
	assert.True(c.External())
	id, err := c.WellKnownFolderID(t.Context(), "inbox")
	require.NoError(err)
	assert.Equal("in", id)
	assert.EqualValues(1, refreshes.Load(), "the rejected bearer is refreshed once")

	// A delta link from Graph is an absolute graph.microsoft.com URL.
	_, err = c.DeltaPage(t.Context(), GraphBaseURL+"/me/mailFolders/A%2FB%3D/messages/delta?$deltatoken=tok1")
	require.NoError(err)
	raw, err := c.GetMIME(t.Context(), "m1")
	require.NoError(err)
	assert.Equal("MIME", string(raw))
	_, err = c.LookupMessage(t.Context(), "missing")
	require.ErrorIs(err, msgraph.ErrNotFound)
	assert.Contains(seen, "/api/outlook/vault/v1/me/messages/m1/$value")
}

// A 401 that the proxy relays from Graph (X-Upstream-Status) is not retried
// with a new bearer.
func TestExternalClientUpstreamUnauthorized(t *testing.T) {
	var refreshes atomic.Int32
	socket := credentialSocket(t, &refreshes)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream-Status", "401")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()
	c, err := NewExternalClient(ExternalIn{Account: "alice@example.com", CredentialSocket: socket, Endpoint: upstream.URL + "/v1"})
	require.NoError(t, err)
	_, err = c.WellKnownFolderID(t.Context(), "inbox")
	require.ErrorIs(t, err, msgraph.ErrUnauthorized)
	assert.EqualValues(t, 0, refreshes.Load())
}

// The transport sends only mailbox reads to the proxy: no writes, no other
// Graph paths, no other hosts, and no redirects, so the bearer cannot leak.
func TestExternalClientRefusesOtherRequests(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var refreshes atomic.Int32
	socket := credentialSocket(t, &refreshes)
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Redirect(w, &http.Request{}, other.URL+"/steal", http.StatusFound)
	}))
	defer upstream.Close()
	c, err := NewExternalClient(ExternalIn{Account: "alice@example.com", CredentialSocket: socket, Endpoint: upstream.URL + "/v1"})
	require.NoError(err)

	require.Error(c.TrashMessage(t.Context(), "m1"))
	require.Error(c.DeleteMessage(t.Context(), "m1"))
	_, err = c.GetRaw(t.Context(), "/users/bob@example.com/messages")
	require.Error(err)
	_, err = c.DeltaPage(t.Context(), other.URL+"/v1.0/me/mailFolders/x/messages/delta")
	require.Error(err)
	for _, path := range []string{"/me/../users/other/messages", "/me/%2e%2e/users/other/messages", "/me/messages/m1?bad=%zz"} {
		_, err := c.GetRaw(t.Context(), path)
		require.Error(err)
	}
	assert.EqualValues(0, requests.Load(), "refused requests never reach the proxy")

	_, err = c.WellKnownFolderID(t.Context(), "inbox")
	var status *msgraph.StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusFound, status.StatusCode)
	assert.EqualValues(1, requests.Load())
	assert.EqualValues(0, elsewhere.Load(), "a redirect is not followed")
}

func TestExternalEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/v1", "https://user:secret@example.com/v1", "https://example.com/v1?token=secret", "https://example.com/v1#x", "/relative", ""} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := NewExternalClient(ExternalIn{Account: "alice@example.com", CredentialSocket: "/tmp/socket", Endpoint: endpoint})
			require.Error(t, err)
		})
	}
	_, err := NewExternalClient(ExternalIn{Account: "", CredentialSocket: "/tmp/socket", Endpoint: "https://example.com/v1"})
	require.Error(t, err)
	_, err = NewExternalClient(ExternalIn{Account: "alice@example.com", CredentialSocket: "/tmp/socket", Endpoint: "http://127.0.0.1:1/v1"})
	require.NoError(t, err)
}

// Credentials that the provider cannot give stop the request.
func TestExternalClientCredentialFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Chdir(t.TempDir())
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", "token.sock")
	require.NoError(err)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer upstream.Close()
	c, err := NewExternalClient(ExternalIn{Account: "alice@example.com", CredentialSocket: "token.sock", Endpoint: upstream.URL + "/v1"})
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err = c.WellKnownFolderID(ctx, "inbox")
	require.ErrorContains(err, "external credentials unavailable")
	assert.EqualValues(0, requests.Load())
}
