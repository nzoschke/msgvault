package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

func TestSyncRunResultsFullAndRepeated(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	seedMessages(env, 3, 12345, "one", "two", "three")
	env.SetOptions(t, func(opts *Options) { opts.Limit = 3 })
	for range 2 {
		summary := runFullSync(t, env)
		page, err := env.Store.ListSyncRunResults(t.Context(), summary.SyncRunID, 0, 100)
		require.NoError(t, err)
		require.Len(t, page.Items, 3)
		ids := make([]string, 0, 3)
		for _, item := range page.Items {
			assert.Equal(t, store.SyncRunItemStatusSuccess, item.Status)
			require.NotNil(t, item.MessageID)
			ids = append(ids, item.SourceMessageID)
		}
		assert.ElementsMatch(t, []string{"one", "two", "three"}, ids)
		assert.Equal(t, store.SyncStatusCompleted, page.RunStatus)
	}
}

func TestSyncRunResultsIncremental(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	seedMessages(env, 1, 1000, "existing")
	runFullSync(t, env)
	env.Mock.AddMessage("new", testMIME(), []string{"INBOX"})
	env.SetHistory(2000, gmail.HistoryRecord{
		ID:            2000,
		MessagesAdded: []gmail.HistoryMessage{{Message: gmail.MessageID{ID: "new"}}},
		LabelsAdded:   []gmail.HistoryLabelChange{{Message: gmail.MessageID{ID: "existing"}, LabelIDs: []string{"SENT"}}},
	})
	summary := runIncrementalSync(t, env)
	page, err := env.Store.ListSyncRunResults(t.Context(), summary.SyncRunID, 0, 100)
	require.NoError(t, err)
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		assert.Equal(t, store.SyncRunItemStatusSuccess, item.Status)
		ids = append(ids, item.SourceMessageID)
	}
	assert.ElementsMatch(t, []string{"existing", "new"}, ids)
}

func TestSyncRunResultsFailureStopsCompletion(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	seedMessages(env, 1, 1000, "one")
	_, err := env.Store.DB().Exec(`CREATE TRIGGER reject_success BEFORE INSERT ON sync_run_items
		WHEN NEW.status = 'success' BEGIN SELECT RAISE(ABORT, 'results unavailable'); END`)
	require.NoError(t, err)
	_, err = env.Syncer.Full(t.Context(), testEmail)
	require.ErrorContains(t, err, "record sync messages")
	source, err := env.Store.GetOrCreateSource("gmail", testEmail)
	require.NoError(t, err)
	assert.False(t, source.SyncCursor.Valid)
	run, err := env.Store.GetLatestSync(source.ID)
	require.NoError(t, err)
	assert.Equal(t, store.SyncStatusFailed, run.Status)
}
