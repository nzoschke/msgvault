package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/gmail"
	msgsync "go.kenn.io/msgvault/internal/sync"
)

func newSyncExternalCmd() *cobra.Command {
	var in gmail.ExternalIn
	command := &cobra.Command{
		Use:   "sync-external email",
		Short: "Back up Gmail through an external credential provider",
		Long: `Back up a Gmail account through a read-only Gmail-compatible endpoint.
The credential socket must respond to GET /token?account=email with JSON
{"access_token":"..."}. On an authentication failure it is called with
refresh=true. Credentials stay in memory. Full sync resumes automatically;
subsequent invocations use incremental sync. Output includes JSON progress
and a terminal sync summary. Interrupted or incomplete runs exit nonzero.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args, nil)
			}
			in.Account = args[0]
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			client, err := gmail.NewExternalClient(in, gmail.WithConcurrency(2), gmail.WithRateLimiter(gmail.NewRateLimiter(2)))
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			profile, err := client.GetProfile(ctx)
			if err != nil {
				return fmt.Errorf("verify account: %w", err)
			}
			if !strings.EqualFold(profile.EmailAddress, in.Account) {
				return errors.New("external account profile does not match requested mailbox")
			}
			st, cleanup, err := openWritableStoreAndInitForIngest()
			if err != nil {
				return err
			}
			defer cleanup()
			source, err := st.GetOrCreateSource("gmail", in.Account)
			if err != nil {
				return err
			}
			opts := msgsync.DefaultOptions()
			opts.AttachmentsDir = cfg.AttachmentsDir()
			opts.BatchSize = 2
			syncer := msgsync.New(client, st, opts).WithLogger(logger).WithProgress(&externalProgress{output: cmd.OutOrStdout()})
			var summary *gmail.SyncSummary
			if !source.SyncCursor.Valid || source.SyncCursor.String == "" {
				summary, err = syncer.Full(ctx, in.Account)
			} else {
				summary, err = syncer.IncrementalWithHistoryRecovery(ctx, source, nil)
			}
			if err != nil {
				return fmt.Errorf("external sync: %w", err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if summary == nil || summary.Errors > 0 {
				return errors.New("external sync has unresolved message errors")
			}
			if err := rebuildCacheAfterWrite(cfg.DatabaseDSN()); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(externalResult{BytesDownloaded: summary.BytesDownloaded, Errors: summary.Errors, FinalHistoryID: summary.FinalHistoryID, MessagesAdded: summary.MessagesAdded, MessagesFound: summary.MessagesFound, MessagesSkipped: summary.MessagesSkipped, MessagesUpdated: summary.MessagesUpdated, SyncRunID: summary.SyncRunID})
		},
	}
	command.Flags().StringVar(&in.Endpoint, "endpoint", "", "Gmail-compatible API base URL ending in /v1")
	command.Flags().StringVar(&in.CredentialSocket, "credential-socket", "", "Unix socket providing GET /token?account=email credentials")
	command.Flags().BoolVar(&in.IncludeSpamTrash, "include-spam-trash", false, "Include Spam and Trash in the full backup")
	_ = command.MarkFlagRequired("endpoint")
	_ = command.MarkFlagRequired("credential-socket")
	return command
}

func init() { rootCmd.AddCommand(newSyncExternalCmd()) }

type externalProgress struct {
	mu     sync.Mutex
	output io.Writer
}

type externalResult struct {
	BytesDownloaded int64  `json:"bytes_downloaded"`
	Errors          int64  `json:"errors"`
	FinalHistoryID  uint64 `json:"final_history_id"`
	MessagesAdded   int64  `json:"messages_added"`
	MessagesFound   int64  `json:"messages_found"`
	MessagesSkipped int64  `json:"messages_skipped"`
	MessagesUpdated int64  `json:"messages_updated"`
	SyncRunID       int64  `json:"sync_run_id"`
}

type externalProgressEvent struct {
	Added     int64  `json:"added,omitempty"`
	Event     string `json:"event"`
	Processed int64  `json:"processed,omitempty"`
	Skipped   int64  `json:"skipped,omitempty"`
	Total     int64  `json:"total,omitempty"`
}

func (p *externalProgress) emit(value externalProgressEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := json.NewEncoder(p.output).Encode(value); err != nil {
		logger.Warn("write external sync progress", "error", err)
	}
}
func (p *externalProgress) OnStart(total int64) {
	p.emit(externalProgressEvent{Event: "started", Total: total})
}
func (p *externalProgress) OnProgress(processed, added, skipped int64) {
	p.emit(externalProgressEvent{Event: "progress", Processed: processed, Added: added, Skipped: skipped})
}
func (p *externalProgress) OnComplete(*gmail.SyncSummary) {}
func (p *externalProgress) OnError(error)                 { p.emit(externalProgressEvent{Event: "message_error"}) }
