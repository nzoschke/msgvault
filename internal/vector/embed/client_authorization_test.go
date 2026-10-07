package embed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
				fmt.Fprint(w, `{"data":[{"index":0,"embedding":[0.1,0.2]}]}`)
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
			_, err := client.Embed(context.Background(), []string{"synthetic test"})
			if tt.missing || tt.wrongEndpoint || tt.redirect {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tt.missing || tt.wrongEndpoint {
				assert.Zero(t, calls)
			} else {
				assert.Equal(t, 1, calls)
			}
			assert.Zero(t, targetCalls)
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
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					assert.Equal(t, "Basic initial", r.Header.Get("Authorization"))
					next := "Basic rotated"
					if tt.revoke {
						next = ""
					}
					assert.NoError(t, os.Setenv("TEST_EMBED_AUTH", next))
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
				require.ErrorContains(t, err, "embedding authorization is unavailable")
				assert.Equal(t, 1, calls)
			} else {
				require.NoError(t, err)
				assert.Equal(t, 2, calls)
			}
		})
	}
}
