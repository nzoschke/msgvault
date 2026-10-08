package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestSyncRunResults(t *testing.T) {
	t.Parallel()
	srv, st := newChangesServer(t)
	archiveIDs := seedChangedMessages(t, st, 3)
	source, err := st.GetOrCreateSource("gmail", "changes@example.com")
	require.NoError(t, err)
	runID, err := st.StartSync(source.ID, "full")
	require.NoError(t, err)
	scoped := st.ScopedToSync(source.ID, runID)
	require.NoError(t, st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: runID,
		SourceMessageID: "changes-msg-3", Status: store.SyncRunItemStatusError, Phase: "fetch", ErrorKind: "fetch_error"}))
	ids := []string{"changes-msg-1", "changes-msg-2", "changes-msg-3", "absent"}
	for range 2 {
		require.NoError(t, scoped.RecordSyncRunMessages(t.Context(), runID, source.ID, ids))
	}
	require.NoError(t, st.CompleteSync(runID, "1000"))
	var results []store.SyncRunResult
	after := int64(0)
	for range 4 {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/sync-runs/%d/items?after_id=%d&limit=1", runID, after), nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var page store.SyncRunResults
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
		assert.Equal(t, runID, page.SyncRunID)
		assert.Equal(t, source.ID, page.SourceID)
		assert.Equal(t, store.SyncStatusCompleted, page.RunStatus)
		results = append(results, page.Items...)
		after = page.NextAfterID
	}
	require.Len(t, results, 3)
	assert.Equal(t, store.SyncRunItemStatusError, results[0].Status)
	assert.Equal(t, store.SyncRunItemStatusSuccess, results[1].Status)
	assert.Equal(t, store.SyncRunItemStatusSuccess, results[2].Status)
	assert.Equal(t, archiveIDs[0], *results[1].MessageID)
	assert.Equal(t, archiveIDs[1], *results[2].MessageID)
	require.ErrorIs(t, scoped.RecordSyncRunMessages(t.Context(), runID, source.ID, ids), store.ErrSyncRunSuperseded)
}

func TestSyncRunResultsInvalidRequest(t *testing.T) {
	t.Parallel()
	srv, _ := newChangesServer(t)
	for _, test := range []struct {
		path   string
		status int
	}{
		{"/0/items", http.StatusBadRequest},
		{"/nope/items", http.StatusBadRequest},
		{"/1/items?after_id=-1", http.StatusBadRequest},
		{"/1/items?limit=0", http.StatusBadRequest},
		{"/1/items?limit=1001", http.StatusBadRequest},
		{"/1/items", http.StatusNotFound},
	} {
		t.Run(test.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sync-runs"+test.path, nil))
			assert.Equal(t, test.status, w.Code, w.Body.String())
		})
	}
}

func TestSyncRunResultsLargePageAndSourceScope(t *testing.T) {
	t.Parallel()
	srv, st := newChangesServer(t)
	archiveIDs := seedChangedMessages(t, st, 501)
	source, err := st.GetOrCreateSource("gmail", "changes@example.com")
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(t, err)
	otherConversation, err := st.EnsureConversationWithType(other.ID, "other", "email_thread", "Other")
	require.NoError(t, err)
	_, err = st.UpsertMessage(&store.Message{SourceID: other.ID, SourceMessageID: "other-only", ConversationID: otherConversation, MessageType: "email"})
	require.NoError(t, err)
	runID, err := st.StartSync(source.ID, "full")
	require.NoError(t, err)
	ids := []string{"other-only"}
	for i := 1; i <= 501; i++ {
		ids = append(ids, fmt.Sprintf("changes-msg-%d", i))
	}
	scoped := st.ScopedToSync(source.ID, runID)
	require.NoError(t, scoped.RecordSyncRunMessages(t.Context(), runID, source.ID, ids))
	page, err := st.ListSyncRunResults(t.Context(), runID, 0, 1000)
	require.NoError(t, err)
	require.Len(t, page.Items, 501)
	assert.Equal(t, store.SyncStatusRunning, page.RunStatus)
	for _, item := range page.Items {
		require.NotNil(t, item.MessageID)
		assert.Contains(t, archiveIDs, *item.MessageID)
	}
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM messages WHERE id = ?`), archiveIDs[0])
	require.NoError(t, err)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/sync-runs/%d/items?limit=1000", runID), nil))
	require.Equal(t, http.StatusOK, w.Code)
	var wire generated.SyncRunResults
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &wire))
	require.Len(t, wire.Items, 501)
	for _, item := range wire.Items {
		if item.SourceMessageID == "changes-msg-1" {
			assert.Nil(t, item.MessageID)
		}
	}
}
