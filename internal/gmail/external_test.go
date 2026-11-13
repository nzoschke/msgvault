package gmail_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/credentialprovider"
)

func TestExternalClient(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var rejected, relayed atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/vault/v1/users/me/messages", r.URL.Path)
		assert.Equal("alice@example.com", r.URL.Query().Get("account"))
		assert.Equal("true", r.URL.Query().Get("includeSpamTrash"))
		switch r.Header.Get("Authorization") {
		case "Bearer fresh":
			_, _ = w.Write([]byte(`{"messages":[{"id":"abc","threadId":"def"}],"nextPageToken":"page2"}`))
		case "Bearer relayed":
			relayed.Add(1)
			w.Header().Set("X-Upstream-Status", "401")
			w.WriteHeader(http.StatusUnauthorized)
		default:
			rejected.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()
	client, err := gmail.NewExternalClient(gmail.ExternalIn{
		Account: "alice@example.com", CredentialProvider: testutil.InboundCredentialCommand(t, "expired", "fresh"),
		Endpoint: upstream.URL + "/vault/v1", IncludeSpamTrash: true,
	})
	require.NoError(err)
	page, err := client.ListMessages(t.Context(), "", "")
	require.NoError(err)
	assert.Equal("page2", page.NextPageToken)
	assert.Equal([]gmail.MessageID{{ID: "abc", ThreadID: "def"}}, page.Messages)
	assert.Equal(int32(1), rejected.Load(), "a rejected bearer renews once")
	require.Error(client.DeleteMessage(t.Context(), "abc"))

	// A Gmail 401 relayed by the endpoint is not a credential failure.
	client, err = gmail.NewExternalClient(gmail.ExternalIn{
		Account: "alice@example.com", CredentialProvider: testutil.InboundCredentialCommand(t, "relayed", "fresh"),
		Endpoint: upstream.URL + "/vault/v1", IncludeSpamTrash: true,
	})
	require.NoError(err)
	_, err = client.ListMessages(t.Context(), "", "")
	require.Error(err)
	assert.Positive(relayed.Load())
	assert.Equal(int32(1), rejected.Load(), "relayed 401s never renew")
}

func TestExternalClientEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/v1", "https://user:secret@example.com/v1", "https://example.com/v1?token=secret", "/relative"} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := gmail.NewExternalClient(gmail.ExternalIn{Account: "alice@example.com", CredentialProvider: []string{"/usr/bin/true"}, Endpoint: endpoint})
			require.Error(t, err)
		})
	}
	_, err := gmail.NewExternalClient(gmail.ExternalIn{Account: "alice@example.com", Endpoint: "https://example.com/v1"})
	require.Error(t, err, "a credential provider is required")
}

func TestExternalClientStopsOnPermanentRefusal(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer upstream.Close()
	for _, tt := range []struct {
		status    credentialprovider.Status
		permanent bool
	}{
		{credentialprovider.StatusSignInRequired, true},
		{credentialprovider.StatusApprovalRequired, true},
		{credentialprovider.StatusUnavailable, false},
	} {
		t.Run(string(tt.status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			client, err := gmail.NewExternalClient(gmail.ExternalIn{
				Account: "alice@example.com", CredentialProvider: testutil.InboundCredentialCommand(t, string(tt.status)),
				Endpoint: upstream.URL + "/vault/v1",
			})
			require.NoError(err)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			_, err = client.ListMessages(ctx, "", "")
			refusal, ok := errors.AsType[*credentialprovider.RefusalError](err)
			require.True(ok, "expected a refusal, got %v", err)
			assert.Equal(tt.status, refusal.Status)
			_, unavailable := errors.AsType[*gmail.CredentialUnavailableError](err)
			assert.Equal(tt.permanent, unavailable)
			assert.Equal(tt.permanent, !errors.Is(err, context.DeadlineExceeded), "only transient refusals spend the retry budget")
			assert.Zero(requests.Load(), "a refused credential never reaches the endpoint")
		})
	}
}
