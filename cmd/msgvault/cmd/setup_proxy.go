package cmd

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
)

func init() {
	var endpoint, authorizationEnv, endpointEnv string
	command := &cobra.Command{
		Use:   "proxy",
		Short: "Prepare opt-in semantic search through an authenticated proxy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return configureEmbeddingProxy(cfg.ConfigFilePath(), endpoint, authorizationEnv, endpointEnv)
		},
	}
	command.Flags().StringVar(&endpoint, "endpoint", "", "OpenAI-compatible proxy base URL")
	command.Flags().StringVar(&authorizationEnv, "authorization-env", "", "Environment variable containing the Authorization header")
	command.Flags().StringVar(&endpointEnv, "authorization-endpoint-env", "", "Environment variable restricting the credential to its proxy endpoint")
	_ = command.MarkFlagRequired("endpoint")
	_ = command.MarkFlagRequired("authorization-env")
	_ = command.MarkFlagRequired("authorization-endpoint-env")
	setupCmd.AddCommand(command)
}

func configureEmbeddingProxy(path, endpoint, authorizationEnv, endpointEnv string) error {
	snapshot, err := config.ReadConfigFile(path)
	if err != nil {
		return fmt.Errorf("read proxy configuration: %w", err)
	}
	var values map[string]any
	metadata, err := toml.Decode(string(snapshot.Content), &values)
	if err != nil {
		return fmt.Errorf("decode proxy configuration: %w", err)
	}
	defaults := []config.Edit{
		{Key: "vector.enabled", Value: false},
		{Key: "vector.embeddings.api_format", Value: "openai"},
		{Key: "vector.embeddings.endpoint", Value: strings.TrimRight(endpoint, "/")},
		{Key: "vector.embeddings.authorization_env", Value: authorizationEnv},
		{Key: "vector.embeddings.authorization_endpoint_env", Value: endpointEnv},
		{Key: "vector.embeddings.model", Value: "text-embedding-3-small"},
		{Key: "vector.embeddings.dimension", Value: 1536},
		{Key: "vector.embeddings.max_input_chars", Value: 8000},
		{Key: "vector.embed.schedule.cron", Value: "* * * * *"},
		{Key: "vector.embed.schedule.run_after_sync", Value: true},
	}
	var edits []config.Edit
	for _, edit := range defaults {
		if !metadata.IsDefined(strings.Split(edit.Key, ".")...) {
			edits = append(edits, edit)
		}
	}
	if len(edits) == 0 {
		return nil
	}
	_, err = config.EditConfigFilePrivate(path, snapshot.ETag, edits)
	if err != nil {
		return fmt.Errorf("configure embedding proxy: %w", err)
	}
	return nil
}
