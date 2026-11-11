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
			assertions := assert.New(t)
			requirements := require.New(t)

			env := newTestEnv(t)
			seedPagedMessages(env, 8)
			source := env.CreateSource(t)
			env.Mock.Profile.HistoryID = 1000
			opts := DefaultOptions()
			opts.InitialBackfill = true
			opts.Query = tt.query
			for i := range tt.interruptions {
				env.Mock.Profile.HistoryID = uint64(1000 + i)
				_, err := New(&cancelOnSecondListAPI{MockAPI: env.Mock}, env.Store, opts).Full(env.Context, testEmail)
				requirements.ErrorIs(err, context.Canceled)
				run, err := env.Store.GetLatestCheckpointedSync(source.ID)
				requirements.NoError(err)
				assertions.Equal("1000", run.CursorAfter.String)
				refreshed, err := env.Store.GetSourceByID(source.ID)
				requirements.NoError(err)
				assertions.Empty(refreshed.SyncCursor.String)
			}
			if tt.fail {
				env.Mock.GetMessageError["msg1"] = errors.New("fetch unavailable")
				_, err := New(env.Mock, env.Store, opts).Full(env.Context, testEmail)
				requirements.ErrorContains(err, "unresolved message errors")
				refreshed, err := env.Store.GetSourceByID(source.ID)
				requirements.NoError(err)
				assertions.Empty(refreshed.SyncCursor.String)
				delete(env.Mock.GetMessageError, "msg1")
			}
			if tt.interruptions > 0 {
				env.Mock.Profile.HistoryID = 2000
			}
			summary, err := New(env.Mock, env.Store, opts).Full(env.Context, testEmail)
			requirements.NoError(err)
			assertions.Zero(summary.Errors)
			assertions.Equal(uint64(1000), summary.FinalHistoryID)
			refreshed, err := env.Store.GetSourceByID(source.ID)
			requirements.NoError(err)
			assertions.Equal("1000", refreshed.SyncCursor.String)
			env.Mock.AddMessage("new-message", testMIME(), []string{"INBOX"})
			env.SetHistory(2001, gmail.HistoryRecord{ID: 2001, MessagesAdded: []gmail.HistoryMessage{{Message: gmail.MessageID{ID: "new-message", ThreadID: "new-message"}}}})
			incremental, err := New(env.Mock, env.Store, nil).Incremental(env.Context, refreshed)
			requirements.NoError(err)
			assertions.Equal(int64(1), incremental.MessagesAdded)
			refreshed, err = env.Store.GetSourceByID(source.ID)
			requirements.NoError(err)
			assertions.Equal("2001", refreshed.SyncCursor.String)
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
			assertions := assert.New(t)
			requirements := require.New(t)

			env := newTestEnv(t)
			source := env.CreateSourceWithHistory(t, tt.cursor)
			opts := DefaultOptions()
			opts.InitialBackfill = true
			opts.Limit = tt.limit
			opts.Query = "after:2025-09-23"
			_, err := New(env.Mock, env.Store, opts).Full(env.Context, testEmail)
			requirements.ErrorContains(err, "initial backfill requires")
			refreshed, err := env.Store.GetSourceByID(source.ID)
			requirements.NoError(err)
			assertions.Equal(tt.cursor, refreshed.SyncCursor.String)
			assertions.Zero(env.Mock.ListMessagesCalls)
		})
	}
}
