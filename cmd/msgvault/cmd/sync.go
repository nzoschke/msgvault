package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/sync"
)

// isIncrementalSyncSource reports whether 'msgvault sync' downloads src.
func isIncrementalSyncSource(src *store.Source) bool {
	switch src.SourceType {
	case sourceTypeGmail, sourceTypeIMAP, sourceTypeMSMail:
		return true
	}
	return false
}

var syncIncrementalCmd = &cobra.Command{
	Use:     "sync [email]",
	Aliases: []string{"sync-incremental"},
	Short:   "Sync new mail from Gmail, IMAP, and Microsoft accounts",
	Long: `Fetch new and changed mail for email accounts. Gmail uses the History API
after a first 'sync-full'; IMAP compares folder high-water marks; Microsoft
Graph mail (add-o365 --graph) uses per-folder delta cursors. Chat and meeting
sources have their own sync-<source> commands; file imports are not synced.

With external Gmail credentials, an account without a cursor receives an initial
full backup; later runs sync incrementally.

IMAP accounts use folder-based sync. Unchanged folders are skipped when
UIDVALIDITY/UIDNEXT high water marks are available.

Microsoft Graph mail accounts (add-o365 --graph) use one delta cursor per
folder. The first sync downloads every folder; later syncs fetch only the
changes, including moves and deletes.

If no email is specified, syncs all accounts that have credentials configured.
Ordinary OAuth accounts without tokens or history IDs are skipped.
Use --json for progress events and per-account summaries. Cache refresh runs
as a separate daemon job.

If history is too old (Gmail returns 404), automatically reconciles the complete
mailbox, preserving archived content while repairing source-deletion metadata.`,
	Example: `  msgvault sync                 # Sync all accounts
  msgvault sync you@example.com   # Sync specific account`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if isDaemonCLISubprocess() {
			return runSyncIncrementalLocal(cmd, args)
		}
		return runSyncIncrementalHTTP(cmd, args)
	},
}

func runSyncIncrementalLocal(cmd *cobra.Command, args []string) error {
	syncResults = nil
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil || state.logger == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	logger := state.logger
	selector, selectorSet, err := syncSourceSelector(cmd, args)
	if err != nil {
		return usageErr(cmd, err)
	}
	s, cleanup, err := openWritableStoreAndInitForInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	dbPath := cfg.DatabaseDSN()

	// Set up context with cancellation before any sync calls
	// so Ctrl+C always saves checkpoints.
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(syncTextOutput(), "\nInterrupted. Saving checkpoint...")
		cancel()
	}()

	// Embedding is no longer driven by sync: newly-ingested messages
	// get embed_gen = NULL by column default and the scan-and-fill
	// embed worker (msgvault embeddings build / the serve daemon)
	// picks them up.

	getOAuthMgr := oauthManagerCache(invocationFromCommand(cmd))

	// Determine which accounts to sync.
	type syncTarget struct {
		source *store.Source
		email  string
	}
	var gmailTargets []syncTarget
	var imapTargets []*store.Source
	var msmailTargets []*store.Source
	var syncErrors []string

	if selectorSet {
		// Resolve all sources for the identifier and route
		// each by type, same as sync-full.
		allMatches, legacy, lookupErr := resolveSyncSources(s, selector)
		if lookupErr != nil {
			return lookupErr
		}
		if err := rejectExplicitlyRetiredSyncSources(allMatches, selector, isIncrementalSyncableSource); err != nil {
			return err
		}
		for _, src := range activeSyncSources(allMatches) {
			switch src.SourceType {
			case sourceTypeGmail:
				gmailTargets = append(gmailTargets, syncTarget{source: src, email: src.Identifier})
			case sourceTypeIMAP:
				imapTargets = append(imapTargets, src)
			case sourceTypeMSMail:
				msmailTargets = append(msmailTargets, src)
			}
		}
		if len(gmailTargets) == 0 && len(imapTargets) == 0 && len(msmailTargets) == 0 {
			if len(allMatches) > 0 {
				return fmt.Errorf("%s exists but its source type cannot be synced (only gmail, imap and msmail are supported)", syncSelectorLabel(selector))
			}
			if legacy {
				// Token not in DB — assume Gmail (legacy behaviour).
				if cfg.Gmail.External != nil {
					source := &store.Source{SourceType: sourceTypeGmail, Identifier: selector.Account}
					gmailTargets = []syncTarget{{source: source, email: selector.Account}}
				} else {
					gmailTargets = []syncTarget{{email: selector.Account}}
				}
			}
		}
	} else {
		// Discover all sources.
		allSources, err := s.ListSources("")
		if err != nil {
			return fmt.Errorf("list sources: %w", err)
		}
		if !slices.ContainsFunc(allSources, isIncrementalSyncSource) {
			return errors.New("no email accounts to sync; add one with 'msgvault add-account <gmail>', 'add-imap', or 'add-o365' (file imports are not synced)")
		}
		allSources = activeSyncSources(allSources)
		for _, src := range allSources {
			switch src.SourceType {
			case sourceTypeGmail:
				if cfg.Gmail.External == nil && !cfg.OAuth.HasAnyConfig() {
					fmt.Fprintf(syncTextOutput(), "Skipping %s (OAuth not configured)\n", src.Identifier)
					continue
				}
				appName := sourceOAuthApp(src)
				if cfg.Gmail.External == nil && (!src.SyncCursor.Valid || src.SyncCursor.String == "") {
					fmt.Fprintf(syncTextOutput(), "Skipping %s (no history ID - run 'sync-full' first)\n", src.Identifier)
					continue
				}
				// Service accounts are always ready
				if cfg.Gmail.External == nil && cfg.OAuth.ServiceAccountKeyFor(appName) == "" {
					mgr, mgrErr := getOAuthMgr(ctx, appName)
					if mgrErr != nil {
						syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, mgrErr))
						continue
					}
					info, readErr := mgr.InspectToken(cmd.Context(), src.Identifier)
					if readErr != nil && mgr.CommandTokens() {
						syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, readErr))
						continue
					}
					if !info.Exists {
						fmt.Fprintf(syncTextOutput(), "Skipping %s (no OAuth token - run 'add-account' first)\n", src.Identifier)
						continue
					}
				}
				gmailTargets = append(gmailTargets, syncTarget{source: src, email: src.Identifier})
			case sourceTypeIMAP:
				skipMsg, parseErr := imapSkipReason(src, cfg, logger)
				if parseErr != nil {
					syncErrors = append(syncErrors, fmt.Sprintf("%s: malformed sync_config: %v", src.Identifier, parseErr))
					continue
				}
				if skipMsg != "" {
					fmt.Fprintln(syncTextOutput(), skipMsg)
					continue
				}
				imapTargets = append(imapTargets, src)
			case sourceTypeMSMail:
				if !newGraphMailManager(state).HasToken(src.Identifier) {
					fmt.Fprintf(syncTextOutput(), "Skipping %s (no Microsoft Graph token - run 'add-o365 %s --graph' first)\n", src.Identifier, src.Identifier)
					continue
				}
				msmailTargets = append(msmailTargets, src)
			default:
				continue
			}
		}
		if len(gmailTargets) == 0 && len(imapTargets) == 0 && len(msmailTargets) == 0 {
			if len(syncErrors) > 0 {
				// Surface the collected errors (e.g. broken OAuth config).
				return fmt.Errorf("%s", syncErrors[0])
			}
			return errors.New("no accounts are ready to sync")
		}
	}

	if syncJSON && len(msmailTargets) > 0 {
		return errors.New("--json supports Gmail and IMAP sync; select a Gmail or IMAP account")
	}

	// Sync IMAP sources via folder-based full sync.
	for _, src := range imapTargets {
		if ctx.Err() != nil {
			break
		}
		fmt.Fprintf(syncTextOutput(), "Note: IMAP account %s uses folder-based sync. Unchanged folders are skipped when high water marks are available.\n\n", src.Identifier)
		if err := runFullSync(ctx, s, getOAuthMgr, src, state); err != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, err))
		}
	}

	// Sync Microsoft Graph mail sources. The first run walks every folder.
	for _, src := range msmailTargets {
		if ctx.Err() != nil {
			break
		}
		fmt.Fprintf(syncTextOutput(), "Syncing Microsoft Graph mail for %s\n", src.Identifier)
		sum, err := runMSMailSync(ctx, s, src.Identifier, func(line string) { fmt.Fprintln(syncTextOutput(), line) }, state)
		if err != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, err))
			continue
		}
		writeMSMailSyncSummary(os.Stdout, src.Identifier, sum)
	}

	// Sync Gmail sources via incremental sync.
	for _, target := range gmailTargets {
		if ctx.Err() != nil {
			break
		}
		if target.source == nil {
			syncErrors = append(syncErrors, target.email+": no source found - run 'sync-full' first")
			continue
		}
		if err := runIncrementalSync(ctx, s, getOAuthMgr, target.source, state); err != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", target.email, err))
			continue
		}
	}

	// Rebuild analytics cache.
	cacheErr := rebuildCacheAfterManualSync(dbPath, state)

	if len(syncErrors) > 0 {
		cacheErr = finishSyncJSON(errors.Join(cacheErr, ctx.Err()))
		fmt.Fprintln(syncTextOutput())
		fmt.Fprintln(syncTextOutput(), "Errors:")
		for _, e := range syncErrors {
			fmt.Fprintf(syncTextOutput(), "  %s\n", e)
		}
		return errors.Join(
			fmt.Errorf("%d account(s) failed to sync: %s", len(syncErrors), strings.Join(syncErrors, "; ")),
			cacheErr,
		)
	}

	return finishSyncJSON(errors.Join(cacheErr, ctx.Err()))
}

func runIncrementalSync(ctx context.Context, s *store.Store, getOAuthMgr func(ctx context.Context, _ string) (*oauth.Manager, error), source *store.Source, state *invocation) error {
	if state == nil {
		state = invocationFromContext(ctx)
	}
	if state == nil || state.cfg == nil || state.logger == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	logger := state.logger
	if !source.SyncCursor.Valid || source.SyncCursor.String == "" {
		if cfg.Gmail.External != nil {
			return runFullSync(ctx, s, getOAuthMgr, source, state)
		}
		return errors.New("no history ID - run 'sync-full' first")
	}

	email := source.Identifier
	client, err := buildAPIClient(ctx, source, getOAuthMgr, nil)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// Set up sync options
	opts := sync.DefaultOptions()
	opts.AttachmentsDir = cfg.AttachmentsDir()

	// Create syncer with progress reporter
	syncer := newMessageSyncer(client, s, opts, state).
		WithLogger(logger).
		WithProgress(syncProgress(state, &CLIProgress{}))

	// Run incremental sync
	startTime := time.Now()
	fmt.Fprintf(syncTextOutput(), "Starting incremental sync for %s\n", email)
	fmt.Fprintf(syncTextOutput(), "Last history ID: %s\n", source.SyncCursor.String)

	summary, err := syncer.IncrementalWithHistoryRecovery(ctx, source, func(resumed bool) {
		if resumed {
			fmt.Fprintln(syncTextOutput(), "Resuming complete mailbox reconciliation after interruption.")
		} else {
			fmt.Fprintln(syncTextOutput(), "History ID has expired (Gmail keeps only ~7 days of history).")
			fmt.Fprintln(syncTextOutput(), "Reconciling the complete mailbox; already-archived messages are skipped.")
		}
		fmt.Fprintln(syncTextOutput())
	})
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(syncTextOutput(), "\nSync interrupted. Run again to resume.")
			return ctx.Err()
		}
		return fmt.Errorf("sync failed: %w", err)
	}

	if syncJSON {
		recordSyncJSON(summary, source)
		if summary.Errors > 0 {
			return errors.New("sync has unresolved message errors")
		}
		return ctx.Err()
	}
	// Print summary
	fmt.Fprintln(syncTextOutput())
	fmt.Fprintln(syncTextOutput(), "Sync complete!")
	fmt.Fprintf(syncTextOutput(), "  Duration:      %s\n", summary.Duration.Round(time.Second))
	fmt.Fprintf(syncTextOutput(), "  Changes:       %d processed, %d added\n",
		summary.MessagesFound, summary.MessagesAdded)
	fmt.Fprintf(syncTextOutput(), "  Downloaded:    %.2f MB\n", float64(summary.BytesDownloaded)/(1024*1024))
	if summary.Errors > 0 {
		fmt.Fprintf(syncTextOutput(), "  Errors:        %d\n", summary.Errors)
	}

	elapsed := time.Since(startTime)
	logger.Info("incremental sync completed",
		keyEmail, email,
		"messages_added", summary.MessagesAdded,
		"elapsed", elapsed,
	)

	if summary.Errors > 0 {
		return errors.New("sync has unresolved message errors")
	}
	return ctx.Err()
}

func init() {
	syncIncrementalCmd.Flags().BoolVar(&syncJSON, "json", false, "Emit JSON progress and per-account sync summaries")
	syncIncrementalCmd.Flags().Int64("source-id", 0, "Exact source ID to sync")
	syncIncrementalCmd.Flags().StringArrayVar(&syncFolders, "folder", []string{}, "IMAP folder to scan; either folder flag replaces saved exclusions for this run (repeatable)")
	syncIncrementalCmd.Flags().StringArrayVar(&syncSkipFolders, "skip-folder", []string{}, "IMAP folder to skip; either folder flag replaces saved exclusions for this run (repeatable)")
	rootCmd.AddCommand(addManualSyncCacheFlags(syncIncrementalCmd))
}
