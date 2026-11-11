package msmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/msgraph"
)

// ExternalIn configures a mail client whose Graph requests go through an
// external read-only proxy. The proxy holds the Microsoft tokens; a local
// credential provider on a Unix socket returns a bearer for the proxy.
type ExternalIn struct {
	Account          string
	CredentialSocket string
	Endpoint         string
	QPS              float64
}

// ExternalProvider is the provider name sent to the credential socket.
const ExternalProvider = "outlook"

// graphMailPrefix is the only Graph path prefix the external transport sends.
const graphMailPrefix = "/v1.0/me/"

type externalTransport struct {
	account     string
	credentials *http.Client
	endpoint    *url.URL
	transport   http.RoundTripper
}

// ValidateExternalEndpoint checks an external proxy base URL. It must be HTTPS
// (plain HTTP only on loopback), absolute, and carry no credentials, query, or
// fragment.
func ValidateExternalEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || endpoint == nil || endpoint.Host == "" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.User != nil {
		return nil, errors.New("invalid external Microsoft endpoint")
	}
	localHTTP := endpoint.Scheme == "http" && (endpoint.Hostname() == "localhost" || net.ParseIP(endpoint.Hostname()).IsLoopback())
	if endpoint.Scheme != "https" && !localHTTP {
		return nil, errors.New("external Microsoft endpoint requires HTTPS")
	}
	return endpoint, nil
}

// NewExternalClient returns a read-only mail Client. Each request must be a
// GET of https://graph.microsoft.com/v1.0/me/...; it is sent to
// Endpoint + "/me/..." with the original query plus account=<Account>, and a
// bearer from the credential socket. On a 401 that the proxy did not relay
// from Graph (no X-Upstream-Status header), the bearer is refreshed once.
func NewExternalClient(in ExternalIn) (*Client, error) {
	endpoint, err := ValidateExternalEndpoint(in.Endpoint)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Account) == "" || strings.TrimSpace(in.CredentialSocket) == "" {
		return nil, errors.New("account and credential socket are required")
	}
	socket := in.CredentialSocket
	transport := &externalTransport{
		account: in.Account, endpoint: endpoint, transport: http.DefaultTransport,
		credentials: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	// The proxy bearer is set by the transport; the Graph token is unused.
	c := NewClient(GraphBaseURL, func(context.Context) (string, error) { return "", nil }, in.QPS)
	c.UseTransport(transport)
	c.external = true
	return c, nil
}

func (t *externalTransport) token(ctx context.Context, refresh bool) (string, error) {
	values := url.Values{"account": {t.account}, "provider": {ExternalProvider}}
	if refresh {
		values.Set("refresh", "true")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://credentials/token?"+values.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := t.credentials.Do(req)
	if err != nil {
		return "", fmt.Errorf("request external credentials: %w: %w", err, msgraph.ErrNotRetryable)
	}
	defer func() { _ = resp.Body.Close() }()
	var credential struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode == http.StatusOK {
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&credential)
	}
	if err != nil || resp.StatusCode != http.StatusOK || credential.AccessToken == "" {
		return "", fmt.Errorf("external credentials unavailable (status %d): %w", resp.StatusCode, msgraph.ErrNotRetryable)
	}
	return credential.AccessToken, nil
}

func (t *externalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	u := req.URL
	if req.Method != http.MethodGet || u.Scheme != "https" || !strings.EqualFold(u.Host, "graph.microsoft.com") || u.User != nil || u.Opaque != "" || u.Fragment != "" || !strings.HasPrefix(u.EscapedPath(), graphMailPrefix) {
		return nil, fmt.Errorf("external Microsoft mail client only permits mailbox reads: %w", msgraph.ErrNotRetryable)
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." || strings.ContainsRune(segment, '\\') {
			return nil, fmt.Errorf("external Microsoft request path is not a mailbox read: %w", msgraph.ErrNotRetryable)
		}
	}
	target := *t.endpoint
	suffix := strings.TrimPrefix(u.EscapedPath(), "/v1.0")
	rawPath := strings.TrimSuffix(t.endpoint.EscapedPath(), "/") + suffix
	path, err := url.PathUnescape(rawPath)
	if err != nil {
		return nil, fmt.Errorf("external Microsoft request path: %w: %w", err, msgraph.ErrNotRetryable)
	}
	target.Path, target.RawPath = path, rawPath
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("external Microsoft request query: %w: %w", err, msgraph.ErrNotRetryable)
	}
	query.Set("account", t.account)
	target.RawQuery = query.Encode()
	for attempt := range 2 {
		bearer, err := t.token(req.Context(), attempt > 0)
		if err != nil {
			return nil, err
		}
		upstream := req.Clone(req.Context())
		upstream.URL = &target
		upstream.Host = target.Host
		upstream.Body, upstream.GetBody, upstream.ContentLength = nil, nil, 0
		upstream.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := t.transport.RoundTrip(upstream)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("X-Upstream-Status") != "" || attempt > 0 {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}
	return nil, fmt.Errorf("external credentials rejected: %w", msgraph.ErrNotRetryable)
}
