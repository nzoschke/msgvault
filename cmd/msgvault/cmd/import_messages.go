package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/messageimport"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newImportMessagesCommand() *cobra.Command {
	return &cobra.Command{
		Use: "import-messages", Short: "Import prepared documents or email projections from JSON on stdin", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), messageimport.MaxRequestBytes+1))
			if err != nil {
				return err
			}
			if len(data) > messageimport.MaxRequestBytes {
				return errors.New("message import exceeds 16 MiB")
			}
			var validated messageimport.ImportMessagesRequest
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&validated); err != nil {
				return errors.New("invalid message import JSON")
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return errors.New("unexpected trailing JSON")
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
			defer client.Close()
			out, err := client.ImportMessages(cmd.Context(), in)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
		},
	}
}

func init() { rootCmd.AddCommand(newImportMessagesCommand()) }
