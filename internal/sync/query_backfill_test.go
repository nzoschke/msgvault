package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryBackfillCheckpointIsolation(t *testing.T) {
	for _, cursor := range []string{"", "123"} {
		for _, tt := range []struct {
			name, nextQuery, scope string
			resume, failure        bool
		}{
			{name: "failed page is replayed", failure: true, nextQuery: "from:a@example.com"},
			{name: "same query", nextQuery: "from:a@example.com", resume: true},
			{name: "different sender", nextQuery: "from:b@example.com"},
			{name: "additional date", nextQuery: "(from:a@example.com) after:2025-09-23"},
			{name: "different spam scope", nextQuery: "from:a@example.com", scope: "external:include-spam-trash"},
		} {
			t.Run(cursor+"/"+tt.name, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)

				env := newTestEnv(t)
				source := env.CreateSource(t)
				if cursor != "" {
					requirements.NoError(env.Store.UpdateSourceSyncCursor(source.ID, cursor))
				}
				before, err := env.Store.GetSourceByID(source.ID)
				requirements.NoError(err)
				seedPagedMessages(env, 8)
				opts := DefaultOptions()
				opts.Query = "from:a@example.com"
				if tt.failure {
					env.Mock.GetMessageError["msg1"] = errors.New("fetch unavailable")
				}
				_, err = New(&cancelOnSecondListAPI{MockAPI: env.Mock}, env.Store, opts).Full(env.Context, testEmail)
				requirements.ErrorIs(err, context.Canceled)
				checkpoint, err := env.Store.GetLatestCheckpointedSync(source.ID)
				requirements.NoError(err)
				requirements.NotEmpty(checkpoint.CursorBefore.String)
				after, err := env.Store.GetSourceByID(source.ID)
				requirements.NoError(err)
				assertions.Equal(before.SyncCursor, after.SyncCursor)

				delete(env.Mock.GetMessageError, "msg1")
				env.Mock.Profile.HistoryID = 2000
				opts.Query = tt.nextQuery
				opts.CheckpointScope = tt.scope
				summary, err := New(env.Mock, env.Store, opts).Full(env.Context, testEmail)
				requirements.NoError(err)
				assertions.Equal(tt.resume, summary.WasResumed)
				if tt.resume {
					assertions.Equal(checkpoint.CursorBefore.String, summary.ResumedFromToken)
				} else {
					assertions.Empty(summary.ResumedFromToken)
				}
				assertions.Zero(summary.Errors)
				assertMessageCount(t, env.Store, 8)
				after, err = env.Store.GetSourceByID(source.ID)
				requirements.NoError(err)
				assertions.Equal(before.SyncCursor, after.SyncCursor)
			})
		}
	}
}

func TestQueryBackfillPreservesAbsentMessages(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	env := newTestEnv(t)
	seedMessages(env, 2, 1000, "match", "outside-query")
	runFullSync(t, env)
	source, err := env.Store.GetSourceByIdentifier(t.Context(), testEmail)
	requirements.NoError(err)
	env.Mock.MessagePages = [][]string{{"match"}}
	delete(env.Mock.Messages, "outside-query")
	env.Mock.Profile.HistoryID = 2000
	opts := DefaultOptions()
	opts.Query = "from:sender@example.com"
	for range 2 {
		summary, err := New(env.Mock, env.Store, opts).Full(env.Context, testEmail)
		requirements.NoError(err)
		assertions.Zero(summary.MessagesAdded)
		assertions.Zero(summary.Errors)
		assertMessageCount(t, env.Store, 2)
		assertDeletedFromSource(t, env.Store, "outside-query", false)
		current, err := env.Store.GetSourceByID(source.ID)
		requirements.NoError(err)
		assertions.Equal(source.SyncCursor, current.SyncCursor)
	}
}

func TestQueryBackfillDoesNotResumeInitialBackfill(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	env := newTestEnv(t)
	source := env.CreateSource(t)
	seedPagedMessages(env, 4)
	opts := DefaultOptions()
	opts.Query = "after:2025-09-23"
	opts.InitialBackfill = true
	_, err := New(&cancelOnSecondListAPI{MockAPI: env.Mock}, env.Store, opts).Full(env.Context, testEmail)
	requirements.ErrorIs(err, context.Canceled)
	opts.InitialBackfill = false
	summary, err := New(env.Mock, env.Store, opts).Full(env.Context, testEmail)
	requirements.NoError(err)
	assertions.False(summary.WasResumed)
	after, err := env.Store.GetSourceByID(source.ID)
	requirements.NoError(err)
	assertions.Equal(source.SyncCursor, after.SyncCursor)
	assertMessageCount(t, env.Store, 4)
}
