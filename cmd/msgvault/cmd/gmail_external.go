package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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
	if external.CredentialProvider.Command == "" {
		return nil, errors.New("external Gmail has no credential provider; run msgvault setup external-gmail --credential-command")
	}
	client, err := gmail.NewExternalClient(gmail.ExternalIn{
		Account: account, Endpoint: external.Endpoint,
		CredentialProvider: external.CredentialProvider.Argv(), IncludeSpamTrash: external.IncludeSpamTrash,
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
	var endpoint, command string
	var args []string
	var includeSpamTrash bool
	setup := &cobra.Command{
		Use: "external-gmail", Short: "Configure an external credential provider for standard Gmail commands",
		Long: `Configure Gmail authentication for sync, sync-full, verify, repair-message,
and daemon jobs. msgvault runs the credential provider command, without a
shell, for each account and speaks the inbound credential provider protocol on
its stdin and stdout. On an authentication failure from the endpoint, msgvault
renews the credential once. Credentials remain in memory. Requests go to the
read-only Gmail-compatible endpoint. Provider writes are unavailable. This
replaces Gmail OAuth for this archive; it does not sync any messages or change
account cursors. Restart a running daemon after changes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if strings.TrimSpace(command) == "" {
				return errors.New("credential provider command must not be empty")
			}
			resolved, err := exec.LookPath(command)
			if err != nil {
				return fmt.Errorf("credential provider command: %w", err)
			}
			if resolved, err = filepath.Abs(resolved); err != nil {
				return err
			}
			endpoint = strings.TrimRight(endpoint, "/")
			// Validate the endpoint and helper scope without running the helper.
			client, err := gmail.NewExternalClient(gmail.ExternalIn{Account: "validation@example.com", Endpoint: endpoint, CredentialProvider: append([]string{resolved}, args...)})
			if err != nil {
				return err
			}
			_ = client.Close()
			snapshot, err := config.ReadConfigFile(state.cfg.ConfigFilePath())
			if err != nil {
				return err
			}
			_, err = config.EditConfigFilePrivate(state.cfg.ConfigFilePath(), snapshot.ETag, []config.Edit{
				{Key: "gmail.external.endpoint", Value: endpoint},
				{Key: "gmail.external.credential_socket", Remove: true},
				{Key: "gmail.external.credential_provider.command", Value: resolved},
				{Key: "gmail.external.credential_provider.args", Value: append([]string{}, args...)},
				{Key: "gmail.external.include_spam_trash", Value: includeSpamTrash},
			})
			return err
		},
	}
	setup.Flags().StringVar(&endpoint, "endpoint", "", "Gmail-compatible API base URL ending in /v1")
	setup.Flags().StringVar(&command, "credential-command", "", "Credential provider executable, resolved to an absolute path")
	setup.Flags().StringArrayVar(&args, "credential-arg", nil, "Argument passed to the credential provider (repeatable)")
	setup.Flags().BoolVar(&includeSpamTrash, "include-spam-trash", false, "Include Spam and Trash when listing messages")
	_ = setup.MarkFlagRequired("endpoint")
	_ = setup.MarkFlagRequired("credential-command")
	return setup
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
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
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
	if err := setDefaultIdentityOptOut(cmd.Context(), cmd, st, source, noDefaultIdentityAddAccount); err != nil {
		return err
	}
	if !noDefaultIdentityAddAccount {
		confirmDefaultIdentity(cmd.Context(), cmd.OutOrStdout(), st, source.ID, email, email, "account-identifier", state.logger)
	}
	if err := runPostSourceCreateMigrationsForInvocation(cmd.Context(), st, state); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Account %s registered with external credentials. Run msgvault sync %s to back up mail.\n", email, email)
	if err != nil {
		return fmt.Errorf("write external account registration: %w", err)
	}
	return nil
}
