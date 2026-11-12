package gmail

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/pkg/credentialprovider"
)

type ExternalIn struct {
	Account            string
	CredentialProvider []string
	Endpoint           string
	IncludeSpamTrash   bool
}

type externalTransport struct {
	account   string
	source    *credentialprovider.InboundSource
	endpoint  *url.URL
	transport http.RoundTripper
}

func NewExternalClient(in ExternalIn, opts ...ClientOption) (*Client, error) {
	endpoint, err := url.Parse(in.Endpoint)
	if err != nil || endpoint == nil || endpoint.Host == "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.User != nil {
		return nil, errors.New("invalid external Gmail endpoint")
	}
	localHTTP := endpoint.Scheme == "http" && (endpoint.Hostname() == "localhost" || net.ParseIP(endpoint.Hostname()).IsLoopback())
	if endpoint.Scheme != "https" && !localHTTP {
		return nil, errors.New("external Gmail endpoint requires HTTPS")
	}
	if in.Account == "" || len(in.CredentialProvider) == 0 || in.CredentialProvider[0] == "" {
		return nil, errors.New("account and credential provider are required")
	}
	source, err := credentialprovider.NewInboundSource(in.CredentialProvider, credentialprovider.InboundScope{
		Endpoint: in.Endpoint, SourceType: "gmail", Account: in.Account,
	})
	if err != nil {
		return nil, errors.New("invalid external Gmail credential provider scope")
	}
	transport := &externalTransport{account: in.Account, source: source, endpoint: endpoint, transport: http.DefaultTransport}
	client := NewClient(nil, opts...)
	client.httpClient = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	client.includeSpamTrash = in.IncludeSpamTrash
	client.externalReadOnly = true
	return client, nil
}

// RoundTrip sends a mailbox read to the external endpoint with a bearer from
// the credential provider. A 401 the endpoint produced itself (no
// X-Upstream-Status) renews the credential once; a relayed Gmail 401 does not.
func (t *externalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || !strings.HasPrefix(req.URL.String(), baseURL+"/users/me/") {
		return nil, errors.New("external Gmail client only permits mailbox reads")
	}
	target := *t.endpoint
	target.Path = strings.TrimSuffix(target.Path, "/") + strings.TrimPrefix(req.URL.Path, "/gmail/v1")
	query := req.URL.Query()
	query.Set("account", t.account)
	target.RawQuery = query.Encode()
	token, err := t.source.Token(req.Context())
	if err != nil {
		return nil, err
	}
	for attempt := range 2 {
		upstream := req.Clone(req.Context())
		upstream.URL = &target
		upstream.Host = target.Host
		upstream.Header.Set("Authorization", "Bearer "+token.Bearer)
		response, err := t.transport.RoundTrip(upstream)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusUnauthorized || response.Header.Get("X-Upstream-Status") != "" || attempt > 0 {
			return response, nil
		}
		_ = response.Body.Close()
		if token, err = t.source.Renew(req.Context(), token); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("external credentials rejected")
}
