package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestConfigureEmbeddingProxy(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		enabled       bool
		cron          string
	}{
		{name: "default off", cron: "* * * * *"},
		{name: "preserve toggle and schedule", content: "[vector]\nenabled = true\n[vector.embed.schedule]\ncron = \"\"\n", enabled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(tt.content), 0600))
			requirements.NoError(configureEmbeddingProxy(path, "https://proxy.example.test/llm/v1", "TEST_AUTH", "TEST_ENDPOINT"))
			loaded, err := config.Load(path, filepath.Dir(path))
			requirements.NoError(err)
			assertions.Equal(tt.enabled, loaded.Vector.Enabled)
			assertions.Equal(tt.cron, loaded.Vector.Embed.Schedule.Cron)
			assertions.Equal("text-embedding-3-small", loaded.Vector.Embeddings.Model)
			assertions.Equal(1536, loaded.Vector.Embeddings.Dimension)
			assertions.Equal("TEST_AUTH", loaded.Vector.Embeddings.AuthorizationEnv)
			before, err := os.ReadFile(path)
			requirements.NoError(err)
			requirements.NoError(configureEmbeddingProxy(path, "https://proxy.example.test/llm/v1", "TEST_AUTH", "TEST_ENDPOINT"))
			after, err := os.ReadFile(path)
			requirements.NoError(err)
			assertions.Equal(string(before), string(after))
		})
	}
}
