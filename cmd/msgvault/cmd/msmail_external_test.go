package cmd

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestSetupExternalMSMail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	require.NoError(os.WriteFile(cfg.ConfigFilePath(), []byte("# keep operator settings\n[microsoft]\nclient_id = \"kept\"\n"), 0o600))
	cmd := newSetupExternalMSMailCmd()
	cmd.SetContext(withStoreResolverConfig(t, cfg))
	cmd.SetArgs([]string{"--endpoint", "https://auth.example.test/api/outlook/vault/v1/", "--credential-socket", filepath.Join(cfg.HomeDir, "credentials.sock")})
	require.NoError(cmd.Execute())
	loaded, err := config.Load(cfg.ConfigFilePath(), cfg.HomeDir)
	require.NoError(err)
	require.NotNil(loaded.Microsoft.External)
	assert.Equal("https://auth.example.test/api/outlook/vault/v1", loaded.Microsoft.External.Endpoint)
	assert.Equal(filepath.Join(cfg.HomeDir, "credentials.sock"), loaded.Microsoft.External.CredentialSocket)
	assert.Equal("kept", loaded.Microsoft.ClientID)
	data, err := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(err)
	assert.Contains(string(data), "# keep operator settings")

	bad := newSetupExternalMSMailCmd()
	bad.SetContext(withStoreResolverConfig(t, cfg))
	bad.SetArgs([]string{"--endpoint", "http://auth.example.test/v1", "--credential-socket", "x.sock"})
	bad.SetOut(io.Discard)
	bad.SetErr(io.Discard)
	require.ErrorContains(bad.Execute(), "HTTPS")
}

// add-o365 --graph registers an msmail source through the external provider,
// with no Microsoft client ID and no sign-in, and refuses an account that has
// an Outlook EML import.
func TestAddO365GraphExternal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	t.Chdir(cfg.HomeDir)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", "credentials.sock")
	require.NoError(err)
	credentials := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("outlook", r.URL.Query().Get("provider"))
		_, _ = io.WriteString(w, `{"access_token":"proxy-bearer"}`)
	})}
	go func() { _ = credentials.Serve(listener) }()
	t.Cleanup(func() { _ = credentials.Close() })
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		assert.Equal("/vault/v1/me/mailFolders/inbox", r.URL.Path)
		assert.Equal("Bearer proxy-bearer", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"id":"inbox-id"}`)
	}))
	defer upstream.Close()
	cfg.Microsoft.External = &config.ExternalMicrosoftConfig{Endpoint: upstream.URL + "/vault/v1", CredentialSocket: "credentials.sock"}

	oldGraph := o365Graph
	t.Cleanup(func() { o365Graph = oldGraph })
	run := func(args ...string) (string, error) {
		cmd := newAddO365LocalCmd()
		cmd.SetContext(withStoreResolverConfig(t, cfg))
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		err := cmd.Execute()
		o365Graph = false
		return out.String(), err
	}

	_, err = run("alice@example.com", "--graph", "--headless")
	require.ErrorContains(err, "--headless is unavailable")
	assert.Zero(probes.Load())

	out, err := run("alice@example.com", "--graph", "--no-default-identity")
	require.NoError(err)
	assert.Contains(out, "registered for Graph mail with external credentials")
	assert.EqualValues(1, probes.Load())

	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	source, err := st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeMSMail, "alice@example.com")
	require.NoError(err)
	assert.Equal("alice@example.com", source.Identifier)

	_, err = st.GetOrCreateSource(sourceTypeOutlookImport, "bob@example.com")
	require.NoError(err)
	_, err = run("bob@example.com", "--graph", "--no-default-identity")
	require.ErrorContains(err, "migrate the existing archive")
	_, err = st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeMSMail, "bob@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound)
}
