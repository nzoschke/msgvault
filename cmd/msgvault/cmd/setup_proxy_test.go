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
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0600))
			require.NoError(t, configureEmbeddingProxy(path, "https://proxy.example.test/llm/v1", "TEST_AUTH", "TEST_ENDPOINT"))
			loaded, err := config.Load(path, filepath.Dir(path))
			require.NoError(t, err)
			assert.Equal(t, tt.enabled, loaded.Vector.Enabled)
			assert.Equal(t, tt.cron, loaded.Vector.Embed.Schedule.Cron)
			assert.Equal(t, "text-embedding-3-small", loaded.Vector.Embeddings.Model)
			assert.Equal(t, 1536, loaded.Vector.Embeddings.Dimension)
			assert.Equal(t, "TEST_AUTH", loaded.Vector.Embeddings.AuthorizationEnv)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, configureEmbeddingProxy(path, "https://proxy.example.test/llm/v1", "TEST_AUTH", "TEST_ENDPOINT"))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}
