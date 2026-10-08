package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
)

// externalGmailClient is shared by foreground sync, daemon jobs, verification,
// and repair. A profile check binds the proxy response to the requested mailbox
// before any archive changes. Credentials are resolved by the provider in memory.
func externalGmailClient(ctx context.Context, account string, state *invocation) (*gmail.Client, error) {
	cfg := state.cfg
	external := cfg.Gmail.External
	client, err := gmail.NewExternalClient(gmail.ExternalIn{
		Account: account, Endpoint: external.Endpoint,
		CredentialSocket: external.CredentialSocket, IncludeSpamTrash: external.IncludeSpamTrash,
	}, gmail.WithLogger(state.logger), gmail.WithRateLimiter(gmail.NewRateLimiter(float64(cfg.Sync.RateLimitQPS))))
	if err != nil {
		return nil, err
	}
	profile, err := client.GetProfile(ctx)
	if err == nil && !strings.EqualFold(profile.EmailAddress, account) {
		err = errors.New("external account profile does not match requested mailbox")
	}
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("verify external account: %w", err)
	}
	return client, nil
}

func newSetupExternalGmailCmd() *cobra.Command {
	var endpoint, socket string
	var includeSpamTrash bool
	command := &cobra.Command{
		Use: "external-gmail", Short: "Configure an external credential provider for standard Gmail commands",
		Long: `Configure Gmail authentication for sync, sync-full, verify, repair-message,
and daemon jobs. The provider resolves each requested account via a protected
Unix socket: GET /token?account=email returns {"access_token":"..."}.
On an authentication failure, msgvault requests refresh=true. Tokens remain in
memory. Requests go to the read-only Gmail-compatible endpoint. Provider writes
are unavailable. This replaces Gmail OAuth for this archive; it does not sync
any messages or change account cursors. Restart a running daemon after changes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if strings.TrimSpace(socket) == "" {
				return errors.New("credential socket must not be empty")
			}
			socket, err := filepath.Abs(socket)
			if err != nil {
				return err
			}
			// Validate the transport without requesting credentials or mailbox data.
			client, err := gmail.NewExternalClient(gmail.ExternalIn{Account: "validation@example.com", Endpoint: endpoint, CredentialSocket: socket})
			if err != nil {
				return err
			}
			_ = client.Close()
			snapshot, err := config.ReadConfigFile(state.cfg.ConfigFilePath())
			if err != nil {
				return err
			}
			_, err = config.EditConfigFilePrivate(state.cfg.ConfigFilePath(), snapshot.ETag, []config.Edit{
				{Key: "gmail.external.endpoint", Value: strings.TrimRight(endpoint, "/")},
				{Key: "gmail.external.credential_socket", Value: socket},
				{Key: "gmail.external.include_spam_trash", Value: includeSpamTrash},
			})
			return err
		},
	}
	command.Flags().StringVar(&endpoint, "endpoint", "", "Gmail-compatible API base URL ending in /v1")
	command.Flags().StringVar(&socket, "credential-socket", "", "Unix socket providing external credentials for each account")
	command.Flags().BoolVar(&includeSpamTrash, "include-spam-trash", false, "Include Spam and Trash when listing messages")
	_ = command.MarkFlagRequired("endpoint")
	_ = command.MarkFlagRequired("credential-socket")
	return command
}

func init() { setupCmd.AddCommand(newSetupExternalGmailCmd()) }

func addExternalGmailAccount(cmd *cobra.Command, email string, state *invocation) error {
	for _, flag := range []string{"oauth-app", "headless", "force", "readonly"} {
		if cmd.Flags().Changed(flag) {
			return usageErr(cmd, fmt.Errorf("--%s is unavailable with an external credential provider; access is managed by the provider", flag))
		}
	}
	client, err := externalGmailClient(cmd.Context(), email, state)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	source, err := st.GetOrCreateSource(sourceTypeGmail, email)
	if err != nil {
		return err
	}
	if accountDisplayName != "" {
		if err := st.UpdateSourceDisplayName(source.ID, accountDisplayName); err != nil {
			return err
		}
	}
	if err := setDefaultIdentityOptOut(cmd, st, source, noDefaultIdentityAddAccount); err != nil {
		return err
	}
	if !noDefaultIdentityAddAccount {
		confirmDefaultIdentity(cmd.OutOrStdout(), st, source.ID, email, email, "account-identifier", state.logger)
	}
	if err := runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Account %s registered with external credentials. Run msgvault sync %s to back up mail.\n", email, email)
	return err
}
