package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/store"
)

var syncJSON bool

type syncJSONProgress struct {
	logger *slog.Logger
	mu     sync.Mutex
	output io.Writer
}

type syncJSONResult struct {
	Account         string `json:"account"`
	SourceID        int64  `json:"source_id"`
	SourceType      string `json:"source_type,omitempty"`
	BytesDownloaded int64  `json:"bytes_downloaded"`
	Errors          int64  `json:"errors"`
	FinalHistoryID  uint64 `json:"final_history_id"`
	MessagesAdded   int64  `json:"messages_added"`
	MessagesFound   int64  `json:"messages_found"`
	MessagesSkipped int64  `json:"messages_skipped"`
	MessagesUpdated int64  `json:"messages_updated"`
	SyncRunID       int64  `json:"sync_run_id"`

	// Microsoft Graph mail only.
	Folders         *int64 `json:"folders,omitzero"`
	MessagesMoved   *int64 `json:"messages_moved,omitzero"`
	MessagesDeleted *int64 `json:"messages_deleted,omitzero"`
}

type syncJSONProgressEvent struct {
	Added     int64  `json:"added,omitzero"`
	Event     string `json:"event"`
	Processed int64  `json:"processed,omitzero"`
	Skipped   int64  `json:"skipped,omitzero"`
	Total     int64  `json:"total,omitzero"`
}

func (p *syncJSONProgress) emit(value syncJSONProgressEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := json.MarshalEncode(jsontext.NewEncoder(p.output), value); err != nil {
		p.logger.Warn("write sync progress", "error", err)
	}
}
func (p *syncJSONProgress) OnStart(total int64) {
	p.emit(syncJSONProgressEvent{Event: "started", Total: total})
}
func (p *syncJSONProgress) OnProgress(processed, added, skipped int64) {
	p.emit(syncJSONProgressEvent{Event: "progress", Processed: processed, Added: added, Skipped: skipped})
}
func (p *syncJSONProgress) OnComplete(*gmail.SyncSummary) {}
func (p *syncJSONProgress) OnError(error)                 { p.emit(syncJSONProgressEvent{Event: "message_error"}) }

var syncResults []syncJSONResult

func syncTextOutput() io.Writer {
	if syncJSON {
		return os.Stderr
	}
	return os.Stdout
}
func syncProgress(state *invocation, fallback gmail.SyncProgress) gmail.SyncProgress {
	if !syncJSON {
		return fallback
	}
	return &syncJSONProgress{output: os.Stdout, logger: state.logger}
}
func recordSyncJSON(s *gmail.SyncSummary, source *store.Source) {
	syncResults = append(syncResults, syncJSONResult{Account: source.Identifier, SourceID: source.ID, BytesDownloaded: s.BytesDownloaded, Errors: s.Errors, FinalHistoryID: s.FinalHistoryID, MessagesAdded: s.MessagesAdded, MessagesFound: s.MessagesFound, MessagesSkipped: s.MessagesSkipped, MessagesUpdated: s.MessagesUpdated, SyncRunID: s.SyncRunID})
}

// recordMSMailSyncJSON reports the counters upstream's Graph sync provides.
func recordMSMailSyncJSON(s *msmail.Summary, source *store.Source) {
	n := func(v int) *int64 { x := int64(v); return &x }
	syncResults = append(syncResults, syncJSONResult{
		Account: source.Identifier, SourceID: source.ID, SourceType: source.SourceType, SyncRunID: s.SyncRunID,
		Errors: int64(s.Errors), MessagesAdded: int64(s.Added), MessagesUpdated: int64(s.Updated),
		Folders: n(s.Folders), MessagesMoved: n(s.Moved), MessagesDeleted: n(s.Deleted),
	})
}

func finishSyncJSON(err error) error {
	for _, result := range syncResults {
		err = errors.Join(err, json.MarshalEncode(jsontext.NewEncoder(os.Stdout), result))
	}
	syncResults = nil
	return err
}
