package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestFullSyncInterruptedFetchResume(t *testing.T) {
	tests := []struct {
		_name            string
		firstPageFailure bool
		interruption     error
		legacyErrors     int64
		resumed          bool
		retryFailure     bool
		savedErrors      int64
	}{
		{_name: "cancelled fetch", interruption: context.Canceled, resumed: true},
		{_name: "deadline", interruption: context.DeadlineExceeded, resumed: true},
		{_name: "legacy cancellation errors", interruption: context.Canceled, legacyErrors: 2},
		{_name: "earlier page failure is retried", interruption: context.Canceled, firstPageFailure: true, savedErrors: 1},
		{_name: "failed batch", interruption: errors.New("unavailable"), savedErrors: 2},
		{_name: "genuine failure remains visible", interruption: context.Canceled, retryFailure: true, resumed: true},
	}
	for _, tt := range tests {
		t.Run(tt._name, func(t *testing.T) {
			a := assert.New(t)
			r := require.New(t)
			env := newTestEnv(t)
			env.Mock.Profile.HistoryID = 12345
			seedPagedMessages(env, 4)
			if tt.firstPageFailure {
				env.Mock.GetMessageError["msg1"] = errors.New("message unavailable")
			}
			env.Syncer = New(&replayControlAPI{MockAPI: env.Mock, batchErrors: map[int]error{1: tt.interruption}}, env.Store, nil)
			_, err := env.Syncer.Full(env.Context, testEmail)
			r.ErrorIs(err, tt.interruption)
			source, err := env.Store.GetSourceByIdentifier(testEmail)
			r.NoError(err)
			run, err := env.Store.GetLatestSync(source.ID)
			r.NoError(err)
			a.Equal(store.SyncStatusFailed, run.Status)
			a.Equal(int64(2), run.MessagesProcessed)
			a.Equal(tt.savedErrors, run.ErrorsCount)
			if tt.legacyErrors > 0 {
				_, err = env.Store.DB().Exec("UPDATE sync_runs SET errors_count = ? WHERE id = ?", tt.legacyErrors, run.ID)
				r.NoError(err)
			}
			delete(env.Mock.GetMessageError, "msg1")
			if tt.retryFailure {
				env.Mock.GetMessageError["msg3"] = errors.New("message unavailable")
			}
			env.Syncer = New(env.Mock, env.Store, nil)
			summary, err := env.Syncer.Full(env.Context, testEmail)
			r.NoError(err)
			a.Equal(tt.resumed, summary.WasResumed)
			expectedErrors := int64(0)
			if tt.retryFailure {
				expectedErrors = 1
			}
			a.Equal(expectedErrors, summary.Errors)
			assertMessageCount(t, env.Store, 4-expectedErrors)
		})
	}
}
