package cmd

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/msmail"
)

func newSetupExternalMSMailCmd() *cobra.Command {
	var endpoint, socket string
	command := &cobra.Command{
		Use:   "external-msmail",
		Short: "Configure an external credential provider for Microsoft Graph mail",
		Long: `Configure Microsoft Graph mail (msmail) authentication for add-o365 --graph,
sync, and daemon jobs. The provider resolves each requested account through a
protected Unix socket: GET /token?account=email&provider=outlook returns
{"access_token":"..."}. On an authentication failure that the endpoint did not
relay from Graph, msgvault requests refresh=true once. Tokens stay in memory.

Requests for https://graph.microsoft.com/v1.0/me/... go to <endpoint>/me/...
with the original query plus account=email. Only GET requests are sent,
redirects are not followed, and provider writes such as delete-staged are
unavailable. This replaces local Microsoft OAuth for msmail sources in this
archive. It does not sync messages or change cursors. Restart a running daemon
after changes.`,
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
			// Validate the transport without requesting credentials or mail.
			if _, err := msmail.NewExternalClient(msmail.ExternalIn{Account: "validation@example.com", Endpoint: endpoint, CredentialSocket: socket}); err != nil {
				return err
			}
			snapshot, err := config.ReadConfigFile(state.cfg.ConfigFilePath())
			if err != nil {
				return err
			}
			_, err = config.EditConfigFilePrivate(state.cfg.ConfigFilePath(), snapshot.ETag, []config.Edit{
				{Key: "microsoft.external.endpoint", Value: strings.TrimRight(endpoint, "/")},
				{Key: "microsoft.external.credential_socket", Value: socket},
			})
			return err
		},
	}
	command.Flags().StringVar(&endpoint, "endpoint", "", "Graph mail proxy base URL; requests go to <endpoint>/me/...")
	command.Flags().StringVar(&socket, "credential-socket", "", "Unix socket providing external credentials for each account")
	_ = command.MarkFlagRequired("endpoint")
	_ = command.MarkFlagRequired("credential-socket")
	return command
}

func init() { setupCmd.AddCommand(newSetupExternalMSMailCmd()) }
