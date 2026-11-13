package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/batchmessageimport"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newImportMessagesCommand() *cobra.Command {
	return &cobra.Command{
		Use: "import-messages", Short: "Import JSON documents or email projections", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), batchmessageimport.MaxRequestBytes+1))
			if err != nil {
				return err
			}
			if len(data) > batchmessageimport.MaxRequestBytes {
				return errors.New("message import exceeds 16 MiB")
			}
			var validated batchmessageimport.ImportMessagesRequest
			if err := json.Unmarshal(data, &validated, json.RejectUnknownMembers(true)); err != nil {
				return errors.New("invalid message import JSON")
			}
			if err := validated.Validate(); err != nil {
				return err
			}
			var in generated.ImportMessagesRequest
			if err := json.Unmarshal(data, &in); err != nil {
				return errors.New("invalid message import JSON")
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			out, err := client.ImportMessages(cmd.Context(), in)
			if err != nil {
				return err
			}
			return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), out)
		},
	}
}

func init() { rootCmd.AddCommand(newImportMessagesCommand()) }
