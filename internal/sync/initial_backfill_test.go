package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
)

func TestInitialBackfill(t *testing.T) {
	tests := []struct {
		_name         string
		query         string
		interruptions int
		fail          bool
	}{
		{_name: "bounded", query: "after:2025-09-23"},
		{_name: "unlimited"},
		{_name: "repeated interruptions retain baseline", query: "after:2025-09-23", interruptions: 2},
		{_name: "fetch failure does not publish cursor", query: "after:2025-09-23", fail: true},
	}
	for _, tt := range tests {
		t.Run(tt._name, func(t *testing.T) {
			env := newTestEnv(t)
			seedPagedMessages(env, 8)
			source := env.CreateSource(t)
			env.Mock.Profile.HistoryID = 1000
			opts := DefaultOptions()
			opts.InitialBackfill = true
			opts.Query = tt.query
			for i := 0; i < tt.interruptions; i++ {
				env.Mock.Profile.HistoryID = uint64(1000 + i)
				_, err := New(&cancelOnSecondListAPI{MockAPI: env.Mock}, env.Store, opts).Full(env.Context, testEmail)
				require.ErrorIs(t, err, context.Canceled)
				run, err := env.Store.GetLatestCheckpointedSync(source.ID)
				require.NoError(t, err)
				assert.Equal(t, "1000", run.CursorAfter.String)
				refreshed, err := env.Store.GetSourceByID(source.ID)
				require.NoError(t, err)
				assert.Empty(t, refreshed.SyncCursor.String)
			}
			if tt.fail {
				env.Mock.GetMessageError["msg1"] = errors.New("fetch unavailable")
				_, err := New(env.Mock, env.Store, opts).Full(env.Context, testEmail)
				require.ErrorContains(t, err, "unresolved message errors")
				refreshed, err := env.Store.GetSourceByID(source.ID)
				require.NoError(t, err)
				assert.Empty(t, refreshed.SyncCursor.String)
				delete(env.Mock.GetMessageError, "msg1")
			}
			if tt.interruptions > 0 {
				env.Mock.Profile.HistoryID = 2000
			}
			summary, err := New(env.Mock, env.Store, opts).Full(env.Context, testEmail)
			require.NoError(t, err)
			assert.Zero(t, summary.Errors)
			assert.Equal(t, uint64(1000), summary.FinalHistoryID)
			refreshed, err := env.Store.GetSourceByID(source.ID)
			require.NoError(t, err)
			assert.Equal(t, "1000", refreshed.SyncCursor.String)
			env.Mock.AddMessage("new-message", testMIME(), []string{"INBOX"})
			env.SetHistory(2001, gmail.HistoryRecord{ID: 2001, MessagesAdded: []gmail.HistoryMessage{{Message: gmail.MessageID{ID: "new-message", ThreadID: "new-message"}}}})
			incremental, err := New(env.Mock, env.Store, nil).Incremental(env.Context, refreshed)
			require.NoError(t, err)
			assert.Equal(t, int64(1), incremental.MessagesAdded)
			refreshed, err = env.Store.GetSourceByID(source.ID)
			require.NoError(t, err)
			assert.Equal(t, "2001", refreshed.SyncCursor.String)
		})
	}
}

func TestInitialBackfillRejectsExistingCursorAndLimit(t *testing.T) {
	tests := []struct {
		_name  string
		cursor string
		limit  int
	}{
		{_name: "existing cursor", cursor: "1000"},
		{_name: "message limit", limit: 10},
	}
	for _, tt := range tests {
		t.Run(tt._name, func(t *testing.T) {
			env := newTestEnv(t)
			source := env.CreateSourceWithHistory(t, tt.cursor)
			opts := DefaultOptions()
			opts.InitialBackfill = true
			opts.Limit = tt.limit
			opts.Query = "after:2025-09-23"
			_, err := New(env.Mock, env.Store, opts).Full(env.Context, testEmail)
			require.ErrorContains(t, err, "initial backfill requires")
			refreshed, err := env.Store.GetSourceByID(source.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.cursor, refreshed.SyncCursor.String)
			assert.Zero(t, env.Mock.ListMessagesCalls)
		})
	}
}
