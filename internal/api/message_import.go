package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/messageimport"
)

type MessageImporter interface {
	ImportMessages(context.Context, messageimport.ImportMessagesRequest) (messageimport.ImportMessagesResponse, error)
}

const messageImportEndpointPath = "/api/v1/import/messages"

func (s *Server) registerMessageImportRoute(api huma.API) {
	op := rawAPIV1Operation("importMessages", http.MethodPost, "/import/messages", "Import documents or project archived emails into a versioned custom source")
	op.RequestBody = jsonRequestBodyFor[messageimport.ImportMessagesRequest](api)
	setCodegenGoType(api.OpenAPI().Components.Schemas.Map()["ImportMessage"].Properties["metadata"], "map[string]any")
	op.Responses = jsonResponsesFor[messageimport.ImportMessagesResponse](api, http.StatusOK)
	op.Errors = []int{400, 401, 409, 413, 415, 422, 500, 503}
	for _, status := range op.Errors {
		op.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	registerRawHumaRoute(api, op, s.handleMessageImport)
}

func (s *Server) handleMessageImport(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != applicationJSONMediaType {
		writeError(w, 415, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, messageimport.MaxRequestBytes))
	decoder.DisallowUnknownFields()
	var in messageimport.ImportMessagesRequest
	err = decoder.Decode(&in)
	if err == nil {
		var extra any
		if tailErr := decoder.Decode(&extra); tailErr != io.EOF {
			err = tailErr
			if err == nil {
				err = errors.New("unexpected trailing JSON")
			}
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, 413, "request_too_large", "Message import exceeds 16 MiB")
		} else {
			writeError(w, 400, "bad_request", "Invalid message import JSON")
		}
		return
	}
	if err := in.Validate(); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	importer, ok := s.store.(MessageImporter)
	if !ok {
		writeError(w, 503, "service_unavailable", "Message import is unavailable")
		return
	}
	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(gateCtx, "message import")
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	out, err := importer.ImportMessages(r.Context(), in)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		if errors.Is(err, messageimport.ErrConflict) {
			writeError(w, 409, "import_conflict", "Source is not import-owned or a record identity has different content")
			return
		}
		if errors.Is(err, messageimport.ErrValidation) {
			writeError(w, 422, "validation_failed", "Invalid message import")
			return
		}
		if s.logger != nil {
			s.logger.Error("message import failed", "error", err)
		}
		writeError(w, 500, "internal_error", "Message import failed")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
