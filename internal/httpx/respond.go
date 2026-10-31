// Package httpx is the HTTP boundary: routing with mandatory permissions, strict decoding and one response shape.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
)

const DefaultMaxBody = 1 << 20

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
}

// WriteJSON writes data as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if data == nil || status == http.StatusNoContent {
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// WriteError writes err as an apperr JSON body; 5xx errors are logged with the request ID.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	e := apperr.From(err)
	if e.Status >= 500 {
		slog.ErrorContext(r.Context(), "request failed", "code", e.Code, "err", err, "requestId", RequestID(r.Context()))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": errorBody{Code: e.Code, Message: e.Message, RequestID: RequestID(r.Context())}})
}

// DecodeJSON accepts exactly one JSON object with no unknown fields.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return DecodeJSONLimit(w, r, dst, DefaultMaxBody)
}

// DecodeJSONLimit is DecodeJSON with a caller-chosen body limit.
func DecodeJSONLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return apperr.ErrUnsupportedMedia
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return apperr.ErrTooLarge
		}
		return apperr.BadRequest("malformed JSON body")
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return apperr.BadRequest("request body must contain a single JSON object")
	}
	if v, ok := dst.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return apperr.BadRequest(err.Error())
		}
	}
	return nil
}
