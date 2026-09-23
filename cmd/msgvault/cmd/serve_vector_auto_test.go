//go:build sqlite_vec

package cmd

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
	"testing"
)

func TestInitialEmbeddingGeneration(t *testing.T) {
	for _, tt := range []struct {
		name                            string
		enabled, auto, building, active bool
		count                           int
	}{
		{name: "disabled", auto: true},
		{name: "manual", enabled: true},
		{name: "first enable", enabled: true, auto: true, count: 1},
		{name: "resume build", enabled: true, auto: true, building: true, count: 1},
		{name: "preserve active", enabled: true, auto: true, active: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := openTestBackend(t)
			c := vector.Config{Enabled: tt.enabled, Embeddings: vector.EmbeddingsConfig{Model: "test-model", Dimension: 4}, Embed: vector.EmbedConfig{AutoInitialize: tt.auto}}
			var existing vector.GenerationID
			if tt.building || tt.active {
				var err error
				existing, err = backend.CreateGeneration(t.Context(), "existing", 4, "existing-fingerprint")
				require.NoError(t, err)
				if tt.active {
					require.NoError(t, backend.ActivateGeneration(t.Context(), existing, true))
				}
			}
			require.NoError(t, ensureInitialEmbeddingGeneration(t.Context(), backend, c))
			require.NoError(t, ensureInitialEmbeddingGeneration(t.Context(), backend, c))
			building, err := backend.BuildingGeneration(t.Context())
			require.NoError(t, err)
			if tt.count == 0 {
				assert.Nil(t, building)
			} else {
				require.NotNil(t, building)
				if tt.building {
					assert.Equal(t, existing, building.ID)
				} else {
					assert.Equal(t, c.GenerationFingerprint(), building.Fingerprint)
				}
			}
			if tt.active {
				active, err := backend.ActiveGeneration(t.Context())
				require.NoError(t, err)
				assert.Equal(t, existing, active.ID)
			}
		})
	}
}
