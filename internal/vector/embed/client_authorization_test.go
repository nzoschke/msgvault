package embed

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientEndpointBoundAuthorization(t *testing.T) {
	for _, tt := range []struct {
		name                             string
		missing, wrongEndpoint, redirect bool
	}{
		{name: "VM credential"},
		{name: "missing credential", missing: true},
		{name: "changed endpoint", wrongEndpoint: true},
		{name: "redirect", redirect: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			calls := 0
			targetCalls := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "Basic dGVzdDpzZWNyZXQ=", r.Header.Get("Authorization"))
				if tt.redirect {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
					return
				}
				_, _ = fmt.Fprint(w, `{"data":[{"index":0,"embedding":[0.1,0.2]}]}`)
			}))
			defer server.Close()
			t.Setenv("TEST_EMBED_AUTH", "Basic dGVzdDpzZWNyZXQ=")
			t.Setenv("TEST_EMBED_ENDPOINT", server.URL)
			if tt.missing {
				t.Setenv("TEST_EMBED_AUTH", "")
			}
			if tt.wrongEndpoint {
				t.Setenv("TEST_EMBED_ENDPOINT", target.URL)
			}
			client := NewClient(Config{Endpoint: server.URL, AuthorizationEnv: "TEST_EMBED_AUTH", AuthorizationEndpointEnv: "TEST_EMBED_ENDPOINT", Model: "text-embedding-3-small", Dimension: 2})
			_, err := client.Embed(t.Context(), []string{"synthetic test"})
			if tt.missing || tt.wrongEndpoint || tt.redirect {
				requirements.Error(err)
			} else {
				requirements.NoError(err)
			}
			if tt.missing || tt.wrongEndpoint {
				assertions.Zero(calls)
			} else {
				assertions.Equal(1, calls)
			}
			assertions.Zero(targetCalls)
		})
	}
}

func TestClientEndpointAuthorizationOnRetry(t *testing.T) {
	for _, tt := range []struct {
		name   string
		revoke bool
	}{
		{name: "rotated credential"},
		{name: "revoked credential", revoke: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					assert.Equal(t, "Basic initial", r.Header.Get("Authorization"))
					next := "Basic rotated"
					if tt.revoke {
						next = ""
					}
					t.Setenv("TEST_EMBED_AUTH", next)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				assert.Equal(t, "Basic rotated", r.Header.Get("Authorization"))
				_, err := fmt.Fprint(w, `{"data":[{"index":0,"embedding":[0.1,0.2]}]}`)
				assert.NoError(t, err)
			}))
			defer server.Close()
			t.Setenv("TEST_EMBED_AUTH", "Basic initial")
			t.Setenv("TEST_EMBED_ENDPOINT", server.URL)
			client := NewClient(Config{Endpoint: server.URL, AuthorizationEnv: "TEST_EMBED_AUTH", AuthorizationEndpointEnv: "TEST_EMBED_ENDPOINT", Model: "text-embedding-3-small", Dimension: 2})
			_, err := client.Embed(t.Context(), []string{"synthetic test"})
			if tt.revoke {
				requirements.ErrorContains(err, "embedding authorization is unavailable")
				assertions.Equal(1, calls)
			} else {
				requirements.NoError(err)
				assertions.Equal(2, calls)
			}
		})
	}
}
