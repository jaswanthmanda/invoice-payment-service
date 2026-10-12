// Package httpx holds shared HTTP helpers. Every error in the API goes through
// WriteError so the error format is identical everywhere:
//
//	{"error": {"code": "...", "message": "...", "details": {...}}}
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const maxBodyBytes = 1 << 20 // 1 MiB

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	WriteJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message, Details: details}})
}

func BadRequest(w http.ResponseWriter, message string) {
	WriteError(w, http.StatusBadRequest, "invalid_request", message, nil)
}

func Unprocessable(w http.ResponseWriter, message string, details map[string]any) {
	WriteError(w, http.StatusUnprocessableEntity, "validation_failed", message, details)
}

func NotFound(w http.ResponseWriter, message string) {
	WriteError(w, http.StatusNotFound, "not_found", message, nil)
}

func Conflict(w http.ResponseWriter, code, message string, details map[string]any) {
	WriteError(w, http.StatusConflict, code, message, details)
}

// Internal logs the real error and returns a generic 500. Never leak err to the client.
func Internal(w http.ResponseWriter, err error) {
	slog.Error("internal error", "err", err)
	WriteError(w, http.StatusInternalServerError, "internal_error", "Internal server error", nil)
}

// DecodeJSON reads a single JSON object into dst. Unknown fields are rejected,
// so a client-supplied "total" on an invoice fails instead of being silently ignored.
// On failure it writes the error response and returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body too large", nil)
		case errors.Is(err, io.EOF):
			BadRequest(w, "Request body is required")
		default:
			BadRequest(w, "Invalid request body: "+err.Error())
		}
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		BadRequest(w, "Request body must contain a single JSON object")
		return false
	}
	return true
}

// PathID parses the {id} path value. A malformed ID is treated as "not found".
func PathID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	return id, err == nil
}

// Page holds cursor pagination params. Lists are ordered newest first
// (UUIDv7 is time-ordered), and After means "rows older than this ID".
type Page struct {
	Limit int
	After *uuid.UUID
}

func ParsePage(w http.ResponseWriter, r *http.Request) (Page, bool) {
	p := Page{Limit: 20}
	q := r.URL.Query()

	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			BadRequest(w, "limit must be an integer between 1 and 100")
			return p, false
		}
		p.Limit = n
	}
	if s := q.Get("starting_after"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			BadRequest(w, "starting_after must be a valid ID")
			return p, false
		}
		p.After = &id
	}
	return p, true
}

func WriteList(w http.ResponseWriter, data any, hasMore bool) {
	WriteJSON(w, http.StatusOK, map[string]any{"data": data, "has_more": hasMore})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Logging assigns a request ID and logs one structured line per request.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := uuid.NewString()
		w.Header().Set("X-Request-Id", reqID)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("request",
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// Recover turns a panic into a 500 in the standard error format.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "err", rec, "path", r.URL.Path)
				WriteError(w, http.StatusInternalServerError, "internal_error", "Internal server error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ErrorBytes builds a standard-format error body without writing it. The pay
// flow needs the bytes so the exact response can be stored for idempotent replay.
func ErrorBytes(code, message string, details map[string]any) []byte {
	b, _ := json.Marshal(errorBody{Error: errorDetail{Code: code, Message: message, Details: details}})
	return b
}

// WriteRaw writes an already-serialized JSON body.
func WriteRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
