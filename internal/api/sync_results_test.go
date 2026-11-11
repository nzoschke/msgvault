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
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	srv, st := newChangesServer(t)
	archiveIDs := seedChangedMessages(t, st, 3)
	source, err := st.GetOrCreateSource("gmail", "changes@example.com")
	requirements.NoError(err)
	runID, err := st.StartSync(source.ID, "full")
	requirements.NoError(err)
	scoped := st.ScopedToSync(source.ID, runID)
	requirements.NoError(st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: runID,
		SourceMessageID: "changes-msg-3", Status: store.SyncRunItemStatusError, Phase: "fetch", ErrorKind: "fetch_error"}))
	ids := []string{"changes-msg-1", "changes-msg-2", "changes-msg-3", "absent"}
	for range 2 {
		requirements.NoError(scoped.RecordSyncRunMessages(t.Context(), runID, source.ID, ids))
	}
	requirements.NoError(st.CompleteSync(runID, "1000"))
	var results []store.SyncRunResult
	after := int64(0)
	for range 4 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/sync-runs/%d/items?after_id=%d&limit=1", runID, after), nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		var page store.SyncRunResults
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &page))
		assertions.Equal(runID, page.SyncRunID)
		assertions.Equal(source.ID, page.SourceID)
		assertions.Equal(store.SyncStatusCompleted, page.RunStatus)
		results = append(results, page.Items...)
		after = page.NextAfterID
	}
	requirements.Len(results, 3)
	assertions.Equal(store.SyncRunItemStatusError, results[0].Status)
	assertions.Equal(store.SyncRunItemStatusSuccess, results[1].Status)
	assertions.Equal(store.SyncRunItemStatusSuccess, results[2].Status)
	assertions.Equal(archiveIDs[0], *results[1].MessageID)
	assertions.Equal(archiveIDs[1], *results[2].MessageID)
	requirements.ErrorIs(scoped.RecordSyncRunMessages(t.Context(), runID, source.ID, ids), store.ErrSyncRunSuperseded)
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
			srv.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sync-runs"+test.path, nil))
			assert.Equal(t, test.status, w.Code, w.Body.String())
		})
	}
}

func TestSyncRunResultsLargePageAndSourceScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	srv, st := newChangesServer(t)
	archiveIDs := seedChangedMessages(t, st, 501)
	source, err := st.GetOrCreateSource("gmail", "changes@example.com")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("gmail", "other@example.com")
	requirements.NoError(err)
	otherConversation, err := st.EnsureConversationWithType(other.ID, "other", "email_thread", "Other")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: other.ID, SourceMessageID: "other-only", ConversationID: otherConversation, MessageType: "email"})
	requirements.NoError(err)
	runID, err := st.StartSync(source.ID, "full")
	requirements.NoError(err)
	ids := []string{"other-only"}
	for i := 1; i <= 501; i++ {
		ids = append(ids, fmt.Sprintf("changes-msg-%d", i))
	}
	scoped := st.ScopedToSync(source.ID, runID)
	requirements.NoError(scoped.RecordSyncRunMessages(t.Context(), runID, source.ID, ids))
	page, err := st.ListSyncRunResults(t.Context(), runID, 0, 1000)
	requirements.NoError(err)
	requirements.Len(page.Items, 501)
	assertions.Equal(store.SyncStatusRunning, page.RunStatus)
	for _, item := range page.Items {
		requirements.NotNil(item.MessageID)
		assertions.Contains(archiveIDs, *item.MessageID)
	}
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM messages WHERE id = ?`), archiveIDs[0])
	requirements.NoError(err)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/sync-runs/%d/items?limit=1000", runID), nil))
	requirements.Equal(http.StatusOK, w.Code)
	var wire generated.SyncRunResults
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &wire))
	requirements.Len(wire.Items, 501)
	for _, item := range wire.Items {
		if item.SourceMessageID == "changes-msg-1" {
			assertions.Nil(item.MessageID)
		}
	}
}
