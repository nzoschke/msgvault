package api

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
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
			req, err := parseCLISyncRequest(httptest.NewRequest("POST", "/?"+tc.query, nil), tc.full)
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
			st := newImportJobTestStore()
			close(st.release)
			server := &Server{store: st}
			response := httptest.NewRecorder()
			server.handleCLISyncFull(response, httptest.NewRequest("POST", "/?"+selector+"&operation_id=attempt-1", nil))
			require.Equal(t, 200, response.Code)
			req := <-st.entered
			assert.Equal(t, "attempt-1", req.OperationID)
			assert.Equal(t, int64(42), req.SourceID)
			assert.True(t, req.SourceIDSet)
			op, err := st.GetSyncOperation("attempt-1")
			require.NoError(t, err)
			assert.Equal(t, "done", op.Status)
			require.Len(t, op.Runs, 1)
			assert.Equal(t, int64(100), op.Runs[0].ID)
		})
	}
}
