// Package httpx holds the small HTTP toolkit shared by every handler:
// error mapping, JSON helpers, validation and middleware.
package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Error is a client-facing error. Handlers return it (or wrap it) and Handle
// renders {"error":{"code","message","fields"}}.
type Error struct {
	Status  int               `json:"-"`
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func NewError(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

func BadRequest(msg string) *Error   { return NewError(http.StatusBadRequest, "bad_request", msg) }
func Unauthorized(msg string) *Error { return NewError(http.StatusUnauthorized, "unauthorized", msg) }
func Forbidden(msg string) *Error    { return NewError(http.StatusForbidden, "forbidden", msg) }
func NotFound(what string) *Error {
	return NewError(http.StatusNotFound, "not_found", what+" not found")
}
func Conflict(code, msg string) *Error { return NewError(http.StatusConflict, code, msg) }

// Unprocessable is used for business-rule failures (e.g. not eligible).
func Unprocessable(code, msg string) *Error {
	return NewError(http.StatusUnprocessableEntity, code, msg)
}

// HandlerFunc is a handler that returns an error instead of writing it.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Handle adapts a HandlerFunc to http.HandlerFunc, translating errors.
func Handle(h HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(w, r, err)
		}
	}
}

func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var he *Error
	var pgErr *pgconn.PgError
	if !errors.As(err, &he) && errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22") {
		// SQLSTATE class 22 is "data exception": the client sent a value the
		// database cannot store (e.g. a NUL byte). That is a bad request, not a server fault.
		he = BadRequest("the request contains a value that cannot be processed")
	}
	if he == nil {
		slog.ErrorContext(r.Context(), "unhandled error",
			"err", err, "method", r.Method, "path", r.URL.Path, "request_id", RequestID(r.Context()))
		he = NewError(http.StatusInternalServerError, "internal", "something went wrong")
	}
	JSON(w, he.Status, map[string]any{"error": he})
}
