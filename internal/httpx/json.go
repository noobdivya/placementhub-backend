package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

const maxJSONBody = 1 << 20 // 1 MiB

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Decode reads a JSON body into v. Unknown fields are ignored so the frontend
// can send whole forms; read-only fields are simply absent from the struct.
func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			return NewError(http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		case errors.Is(err, io.EOF):
			return BadRequest("request body is empty")
		default:
			return BadRequest("invalid JSON body")
		}
	}
	if dec.More() {
		return BadRequest("unexpected data after JSON body")
	}
	return nil
}

// Page describes a list response.
type Page[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
	Page  int `json:"page"`
	Limit int `json:"limit"`
}

// Paging reads ?page= and ?limit= with sane bounds.
func Paging(r *http.Request, defLimit, maxLimit int) (page, limit, offset int) {
	page = atoi(r.URL.Query().Get("page"), 1)
	limit = atoi(r.URL.Query().Get("limit"), defLimit)
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = defLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	return page, limit, (page - 1) * limit
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
