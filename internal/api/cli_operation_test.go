package api

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLISyncOperationParser(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		full, valid bool
	}{
		{"exact email", "email=archive%40example.com&operation_id=attempt-1", true, true},
		{"source ID", "source_id=42&operation_id=attempt_1", true, true},
		{"empty", "email=a&operation_id=", true, false},
		{"duplicate", "email=a&operation_id=a&operation_id=b", true, false},
		{"unsafe", "email=a&operation_id=a%2Fb", true, false},
		{"all sources", "operation_id=a", true, false},
		{"incremental", "email=a&operation_id=a", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parseCLISyncRequest(httptest.NewRequestWithContext(t.Context(), "POST", "/?"+tc.query, nil), tc.full)
			assert.Equal(t, tc.valid, err == nil)
			if tc.valid {
				assert.NotEmpty(t, req.OperationID)
			}
		})
	}
}
func TestCLISyncOperationReservedBeforeExecution(t *testing.T) {
	for _, selector := range []string{"email=archive%40example.com", "source_id=42"} {
		t.Run(selector, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := newImportJobTestStore()
			close(st.release)
			server := &Server{store: st}
			response := httptest.NewRecorder()
			server.handleCLISyncFull(response, httptest.NewRequestWithContext(t.Context(), "POST", "/?"+selector+"&operation_id=attempt-1", nil))
			requirements.Equal(200, response.Code)
			req := <-st.entered
			assertions.Equal("attempt-1", req.OperationID)
			assertions.Equal(int64(42), req.SourceID)
			assertions.True(req.SourceIDSet)
			op, err := st.GetSyncOperation("attempt-1")
			requirements.NoError(err)
			assertions.Equal("done", op.Status)
			requirements.Len(op.Runs, 1)
			assertions.Equal(int64(100), op.Runs[0].ID)
		})
	}
}
