package daemonclient

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestImportJobAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/imports", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, err := w.Write([]byte(`{"job_id":"stable-id","source_id":1,"account":"archive@example.com","status":"pending","sync_run_ids":[],"processed":0,"added":0,"skipped":0,"created_at":"2026-10-01T00:00:00Z","started_at":null,"finished_at":null}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()
	c, err := New(Config{URL: srv.URL, AllowInsecure: true})
	require.NoError(t, err)
	out, err := c.CreateImportJob(t.Context(), generated.ImportJobRequest{Account: "archive@example.com"})
	require.NoError(t, err)
	assert.Equal(t, "stable-id", out.JobID)
}
