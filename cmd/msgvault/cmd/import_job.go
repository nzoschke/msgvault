package cmd

import (
	"encoding/json"
	"errors"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/pkg/client/generated"
	"io"
)

func newImportJobCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "import-job", Short: "Start or inspect a durable historical import job"}
	cmd.AddCommand(&cobra.Command{Use: "start", Short: "Start a job from JSON on stdin; return without waiting", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var in generated.ImportJobRequest
		dec := json.NewDecoder(io.LimitReader(cmd.InOrStdin(), (16<<10)+1))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return err
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return errors.New("unexpected trailing JSON")
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer client.Close()
		out, err := client.CreateImportJob(cmd.Context(), in)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}})
	cmd.AddCommand(&cobra.Command{Use: "status JOB_ID", Short: "Get job status and related sync run IDs", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer client.Close()
		out, err := client.GetImportJob(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}})
	return cmd
}
func init() { rootCmd.AddCommand(newImportJobCommand()) }
