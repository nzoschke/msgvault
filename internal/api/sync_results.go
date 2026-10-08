package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"go.kenn.io/msgvault/internal/store"
)

type SyncRunResultsStore interface {
	ListSyncRunResults(context.Context, int64, int64, int) (*store.SyncRunResults, error)
}

func (s *Server) handleSyncRunResults(w http.ResponseWriter, r *http.Request) {
	syncID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || syncID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_sync_run", "id must be a positive integer")
		return
	}
	afterID := int64(0)
	if value := r.URL.Query().Get("after_id"); value != "" {
		afterID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || afterID < 0 {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "after_id must be a nonnegative integer")
			return
		}
	}
	limit := 100
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 1000 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 1000")
			return
		}
	}
	st, ok := s.store.(SyncRunResultsStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "sync results unavailable")
		return
	}
	page, err := st.ListSyncRunResults(r.Context(), syncID, afterID, limit)
	if errors.Is(err, store.ErrSyncRunNotFound) {
		writeError(w, http.StatusNotFound, "sync_run_not_found", "sync run not found")
		return
	}
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, "sync_results_failed", "could not read sync results")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
