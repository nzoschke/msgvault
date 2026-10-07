package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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

func TestSyncExternalFlagValidation(t *testing.T) {
	for _, args := range [][]string{{"--query", ""}, {"--query", " \t\n"}, {"--after", "yesterday"}} {
		t.Run(strings.Join(args, "="), func(t *testing.T) {
			server, calls := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
			cmd := newSyncExternalCmd()
			cmd.SetContext(configureRemoteDaemonForTest(t, server.URL))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"alice@example.com", "--endpoint", "https://proxy.example.test/v1", "--credential-socket", "/tmp/token.sock"}, args...))
			require.ErrorContains(t, cmd.Execute(), args[0])
			assert.Zero(t, calls.Load(), "validation must precede daemon work")
		})
	}
}

func TestSyncExternalDaemonForwarding(t *testing.T) {
	for _, query := range []string{"from:a@example.com OR from:b@example.com", "in:anywhere rfc822msgid:fixture+abc@example.com"} {
		cmd := newSyncExternalCmd()
		require.NoError(t, cmd.ParseFlags([]string{"alice@example.com", "--endpoint", "https://proxy.example.test/v1", "--credential-socket", "/tmp/token.sock", "--query", query, "--after", "2025-09-23", "--include-spam-trash"}))
		args, err := daemonCLIArgsFromCobra(cmd, cmd.Flags().Args())
		require.NoError(t, err)
		assert.Equal(t, []string{"sync-external", "--after=2025-09-23", "--credential-socket=/tmp/token.sock", "--endpoint=https://proxy.example.test/v1", "--include-spam-trash", "--query=" + query, "alice@example.com"}, args)
	}
}

// Exercise the real external client, command routing, ingestion and cache builder.
func TestSyncExternalBackfill(t *testing.T) {
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
		{name: "date only incremental", cursor: "123", after: "2025-09-23", incremental: true},
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
				cmd := newSyncExternalCmd()
				cmd.SetContext(withStoreResolverConfig(t, cfg))
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(io.Discard)
				args := []string{"alice@example.com", "--endpoint", upstream.URL + "/v1", "--credential-socket", "token.sock"}
				if tt.query != "" {
					args = append(args, "--query", tt.query)
				}
				if tt.after != "" {
					args = append(args, "--after", tt.after)
				}
				if tt.spam {
					args = append(args, "--include-spam-trash")
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
			if tt.query != "" {
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
				cached := cacheNeedsBuild(cfg.DatabaseDSN(), cfg.AnalyticsDir())
				assert.False(t, cached.NeedsBuild, cached.Reason)
			}
		})
	}
}
