package cmd

import (
	"encoding/base64"
	"encoding/json"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestStandardExternalFlagValidation(t *testing.T) {
	for _, args := range [][]string{{"--query", ""}, {"--query", " \t\n"}, {"--after", "yesterday"}} {
		t.Run(strings.Join(args, "="), func(t *testing.T) {
			server, calls := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
			cmd := newStandardExternalTestCmd(t, false)
			cmd.SetContext(configureRemoteDaemonForTest(t, server.URL))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"alice@example.com"}, args...))
			require.ErrorContains(t, cmd.Execute(), args[0])
			assert.Zero(t, calls.Load(), "validation must precede daemon work")
		})
	}
}

func TestStandardExternalDaemonForwarding(t *testing.T) {
	for _, query := range []string{"from:a@example.com OR from:b@example.com", "in:anywhere rfc822msgid:fixture+abc@example.com"} {
		args := cliSyncSubprocessArgs(api.CLISyncRequest{Full: true, JSON: true, Email: "alice@example.com", Query: query, After: "2025-09-23", Before: "2025-09-25", Limit: 3, NoResume: true})
		cmd := newStandardExternalTestCmd(t, false)
		require.NoError(t, cmd.ParseFlags(args[1:]))
		assert.Equal(t, query, syncQuery)
		assert.Equal(t, "2025-09-23", syncAfter)
		assert.Equal(t, "2025-09-25", syncBefore)
		assert.Equal(t, 3, syncLimit)
		assert.True(t, syncNoResume)
		assert.True(t, syncJSON)
	}
}

func newStandardExternalTestCmd(t *testing.T, incremental bool) *cobra.Command {
	oldQuery, oldAfter, oldBefore, oldNoResume, oldJSON, oldLimit := syncQuery, syncAfter, syncBefore, syncNoResume, syncJSON, syncLimit
	t.Cleanup(func() {
		syncQuery, syncAfter, syncBefore, syncNoResume, syncJSON, syncLimit = oldQuery, oldAfter, oldBefore, oldNoResume, oldJSON, oldLimit
	})
	syncQuery, syncAfter, syncBefore = "", "", ""
	syncNoResume, syncJSON, syncLimit = false, false, 0
	run := syncFullCmd.RunE
	name := "sync-full"
	if incremental {
		run = syncIncrementalCmd.RunE
		name = "sync"
	}
	cmd := &cobra.Command{Use: name, Args: cobra.MaximumNArgs(1), RunE: run}
	cmd.Flags().StringVar(&syncQuery, "query", "", "")
	cmd.Flags().StringVar(&syncAfter, "after", "", "")
	cmd.Flags().StringVar(&syncBefore, "before", "", "")
	cmd.Flags().BoolVar(&syncNoResume, "noresume", false, "")
	cmd.Flags().BoolVar(&syncJSON, "json", false, "")
	cmd.Flags().IntVar(&syncLimit, "limit", 0, "")
	cmd.Flags().Int64("source-id", 0, "")
	return addManualSyncCacheFlags(cmd)
}

// Exercise the real external client, command routing, ingestion and cache builder.
func TestStandardExternalBackfill(t *testing.T) {
	for _, tt := range []struct {
		name, cursor, query, after, effective string
		spam, incremental, failure            bool
	}{
		{name: "existing cursor", cursor: "123", query: "from:sender@example.com", effective: "from:sender@example.com"},
		{name: "unset cursor", query: "from:sender@example.com", effective: "from:sender@example.com"},
		{name: "OR with date", cursor: "123", query: "from:a@example.com OR from:b@example.com", after: "2025-09-23", effective: "(from:a@example.com OR from:b@example.com) after:2025-09-23"},
		{name: "grouped OR with date and spam", cursor: "123", query: " (from:a@example.com OR from:b@example.com) ", after: "2025-09-23", effective: "((from:a@example.com OR from:b@example.com)) after:2025-09-23", spam: true},
		{name: "partial failure unset cursor", query: "from:sender@example.com", effective: "from:sender@example.com", failure: true},
		{name: "partial failure", cursor: "123", query: "from:sender@example.com", effective: "from:sender@example.com", failure: true},
		{name: "date only initial", after: "2025-09-23", effective: "after:2025-09-23"},
		{name: "unfiltered initial"},
		{name: "incremental", cursor: "123", incremental: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			markDaemonCLISubprocessForTest(t)
			t.Chdir(t.TempDir())
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(t, err)
			t.Cleanup(func() { _ = st.Close() })
			require.NoError(t, st.InitSchema())
			source, err := st.GetOrCreateSource("gmail", "alice@example.com")
			require.NoError(t, err)
			if tt.cursor != "" {
				require.NoError(t, st.UpdateSourceSyncCursor(source.ID, tt.cursor))
			}
			source, err = st.GetSourceByID(source.ID)
			require.NoError(t, err)
			originalCursor := source.SyncCursor

			listener, err := net.Listen("unix", "token.sock")
			require.NoError(t, err)
			credentials := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "alice@example.com", r.URL.Query().Get("account"))
				_, _ = io.WriteString(w, `{"access_token":"fixture-token"}`)
			})}
			go func() { _ = credentials.Serve(listener) }()
			t.Cleanup(func() { _ = credentials.Close() })
			fail := tt.failure
			var queries []string
			var historyCalls int
			raw := "From: sender@example.com\r\nTo: alice@example.com\r\nSubject: Backfill fixture\r\nMessage-ID: <fixture@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=fixture\r\n\r\n--fixture\r\nContent-Type: text/plain\r\n\r\nArchived body\r\n--fixture\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=note.txt\r\n\r\nAttachment content\r\n--fixture--\r\n"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
				assert.Equal(t, "alice@example.com", r.URL.Query().Get("account"))
				var result any
				switch r.URL.Path {
				case "/v1/users/me/profile":
					result = map[string]any{"emailAddress": "alice@example.com", "historyId": "999", "messagesTotal": 2}
				case "/v1/users/me/labels":
					result = map[string]any{"labels": []any{}}
				case "/v1/users/me/messages":
					queries = append(queries, r.URL.Query().Get("q"))
					assert.Equal(t, tt.effective, r.URL.Query().Get("q"))
					assert.Equal(t, tt.spam, r.URL.Query().Get("includeSpamTrash") == "true")
					result = map[string]any{"messages": []any{map[string]string{"id": "match", "threadId": "thread"}, map[string]string{"id": "other", "threadId": "other"}}}
				case "/v1/users/me/history":
					historyCalls++
					assert.Equal(t, "123", r.URL.Query().Get("startHistoryId"))
					result = map[string]any{"historyId": "999"}
				case "/v1/users/me/messages/match", "/v1/users/me/messages/other":
					if fail && strings.HasSuffix(r.URL.Path, "/other") {
						http.Error(w, "fixture failure", http.StatusBadRequest)
						return
					}
					assert.Equal(t, "raw", r.URL.Query().Get("format"))
					id := filepath.Base(r.URL.Path)
					result = map[string]any{"id": id, "threadId": id, "historyId": "999", "internalDate": "1758585600000", "raw": base64.RawURLEncoding.EncodeToString([]byte(raw))}
				default:
					assert.Fail(t, "unexpected request", "%s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				assert.NoError(t, json.NewEncoder(w).Encode(result))
			}))
			t.Cleanup(upstream.Close)
			run := func() error {
				cmd := newStandardExternalTestCmd(t, tt.incremental)
				cmd.SetContext(withStoreResolverConfig(t, cfg))
				cmd.SetOut(io.Discard)
				cmd.SetErr(io.Discard)
				cfg.Gmail.External = &config.ExternalGmailConfig{Endpoint: upstream.URL + "/v1", CredentialSocket: "token.sock", IncludeSpamTrash: tt.spam}
				args := []string{"alice@example.com", "--build-cache", "--json"}
				if tt.query != "" {
					args = append(args, "--query", tt.query)
				}
				if tt.after != "" {
					args = append(args, "--after", tt.after)
				}

				cmd.SetArgs(args)
				return cmd.Execute()
			}
			if tt.failure {
				require.ErrorContains(t, run(), "unresolved message errors")
				got, err := st.GetSourceByID(source.ID)
				require.NoError(t, err)
				assert.Equal(t, originalCursor, got.SyncCursor)
				fail = false
			}
			require.NoError(t, run())
			if tt.query != "" {
				require.NoError(t, run(), "repeat is idempotent")
			}
			got, err := st.GetSourceByID(source.ID)
			require.NoError(t, err)
			if tt.query != "" || tt.after != "" {
				assert.Equal(t, originalCursor, got.SyncCursor)
			} else {
				assert.Equal(t, "999", got.SyncCursor.String)
			}
			var count int
			require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&count))
			if tt.incremental {
				assert.Zero(t, count)
				assert.Empty(t, queries)
				assert.Equal(t, 1, historyCalls)
			} else {
				assert.Equal(t, 2, count)
				assert.Zero(t, historyCalls)
				var id int64
				require.NoError(t, st.DB().QueryRow("SELECT id FROM messages WHERE source_message_id = 'match'").Scan(&id))
				body, err := st.GetMessageBodyText(id)
				require.NoError(t, err)
				assert.Contains(t, body, "Archived body")
				mime, err := st.GetMessageRaw(id)
				require.NoError(t, err)
				assert.Equal(t, raw, string(mime))
				require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM attachments").Scan(&count))
				assert.Equal(t, 2, count)
				attachments, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
				require.NoError(t, err)
				require.Len(t, attachments, 1)
				content, err := os.ReadFile(filepath.Join(cfg.AttachmentsDir(), attachments[0].StoragePath))
				require.NoError(t, err)
				assert.Contains(t, string(content), "Attachment content")
				// Cache refresh is queued by the parent daemon; checked in integration.
			}
		})
	}
}

func TestSetupExternalGmail(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	require.NoError(t, os.WriteFile(cfg.ConfigFilePath(), []byte("# keep operator settings\n[vector]\nenabled = false\n"), 0600))
	cmd := newSetupExternalGmailCmd()
	cmd.SetContext(withStoreResolverConfig(t, cfg))
	cmd.SetArgs([]string{"--endpoint", "https://proxy.example.test/v1", "--credential-socket", filepath.Join(cfg.HomeDir, "token.sock"), "--include-spam-trash"})
	require.NoError(t, cmd.Execute())
	loaded, err := config.Load(cfg.ConfigFilePath(), cfg.HomeDir)
	require.NoError(t, err)
	require.NotNil(t, loaded.Gmail.External)
	assert.Equal(t, "https://proxy.example.test/v1", loaded.Gmail.External.Endpoint)
	assert.True(t, loaded.Gmail.External.IncludeSpamTrash)
	assert.False(t, loaded.Vector.Enabled)
	data, err := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(t, err)
	assert.Contains(t, string(data), "# keep operator settings")
	assert.NotContains(t, string(data), "access_token")
}

func TestExternalSharedClientRejectsMismatchedAccount(t *testing.T) {
	t.Chdir(t.TempDir())
	listener, err := net.Listen("unix", "token.sock")
	require.NoError(t, err)
	credentials := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"fixture-token"}`)
	})}
	go func() { _ = credentials.Serve(listener) }()
	t.Cleanup(func() { _ = credentials.Close() })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"emailAddress":"other@example.com","historyId":"123"}`)
	}))
	defer upstream.Close()
	cfg := config.NewDefaultConfig()
	cfg.Gmail.External = &config.ExternalGmailConfig{Endpoint: upstream.URL + "/v1", CredentialSocket: "token.sock"}
	ctx := withStoreResolverConfig(t, cfg)
	src := &store.Source{SourceType: "gmail", Identifier: "alice@example.com"}
	_, err = buildAPIClient(ctx, src, nil, nil)
	require.ErrorContains(t, err, "profile does not match")
	_, _, err = newDaemonGmailClient(ctx, src.Identifier, src, nil, invocationFromContext(ctx))
	require.ErrorContains(t, err, "profile does not match")
}
