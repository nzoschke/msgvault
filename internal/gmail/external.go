package gmail

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
)

type ExternalIn struct {
	Account          string
	CredentialSocket string
	Endpoint         string
	IncludeSpamTrash bool
}

type externalTransport struct {
	account     string
	credentials *http.Client
	endpoint    *url.URL
	transport   http.RoundTripper
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
	if in.Account == "" || in.CredentialSocket == "" {
		return nil, errors.New("account and credential socket are required")
	}
	transport := &externalTransport{
		account: in.Account, endpoint: endpoint, transport: http.DefaultTransport,
		credentials: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", in.CredentialSocket)
		}}},
	}
	client := NewClient(nil, opts...)
	client.httpClient = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	client.includeSpamTrash = in.IncludeSpamTrash
	client.externalReadOnly = true
	return client, nil
}

func (t *externalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || !strings.HasPrefix(req.URL.String(), baseURL+"/users/me/") {
		return nil, errors.New("external Gmail client only permits mailbox reads")
	}
	for attempt := range 2 {
		values := url.Values{"account": {t.account}}
		if attempt > 0 {
			values.Set("refresh", "true")
		}
		credentialRequest, err := http.NewRequestWithContext(req.Context(), http.MethodGet, "http://credentials/token?"+values.Encode(), nil)
		if err != nil {
			return nil, err
		}
		response, err := t.credentials.Do(credentialRequest)
		if err != nil {
			return nil, fmt.Errorf("request external credentials: %w", err)
		}
		var credential struct {
			AccessToken string `json:"access_token"`
		}
		if response.StatusCode == http.StatusOK {
			err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&credential)
		}
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || credential.AccessToken == "" {
			return nil, fmt.Errorf("external credentials unavailable (status %d)", response.StatusCode)
		}
		upstream := req.Clone(req.Context())
		target := *t.endpoint
		target.Path = strings.TrimSuffix(target.Path, "/") + strings.TrimPrefix(req.URL.Path, "/gmail/v1")
		query := req.URL.Query()
		query.Set("account", t.account)
		target.RawQuery = query.Encode()
		upstream.URL = &target
		upstream.Host = target.Host
		upstream.Header.Set("Authorization", "Bearer "+credential.AccessToken)
		response, err = t.transport.RoundTrip(upstream)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusUnauthorized || response.Header.Get("X-Upstream-Status") != "" || attempt > 0 {
			return response, nil
		}
		_ = response.Body.Close()
	}
	return nil, errors.New("external credentials rejected")
}
