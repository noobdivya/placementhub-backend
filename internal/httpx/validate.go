package httpx

import (
	"net/http"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// V collects field errors so one response can report every problem.
type V struct{ fields map[string]string }

func (v *V) Add(field, msg string) {
	if v.fields == nil {
		v.fields = map[string]string{}
	}
	if _, ok := v.fields[field]; !ok {
		v.fields[field] = msg
	}
}

// Err returns nil when valid, else a 422 listing every failing field.
func (v *V) Err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return &Error{Status: http.StatusUnprocessableEntity, Code: "validation_failed",
		Message: "some fields are invalid", Fields: v.fields}
}

// Text checks the trimmed rune length is within [min, max].
func (v *V) Text(field, s string, min, max int) {
	n := utf8.RuneCountInString(strings.TrimSpace(s))
	switch {
	case n < min && min == 1:
		v.Add(field, "is required")
	case n < min:
		v.Add(field, "is too short")
	case n > max:
		v.Add(field, "is too long")
	}
}

func (v *V) Email(field, s string) {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || !strings.Contains(s, ".") || len(s) > 254 {
		v.Add(field, "must be a valid email address")
	}
}

func (v *V) Range(field string, n, min, max float64) {
	if n < min || n > max {
		v.Add(field, "is out of range")
	}
}

func (v *V) OneOf(field, s string, allowed ...string) {
	for _, a := range allowed {
		if s == a {
			return
		}
	}
	v.Add(field, "must be one of: "+strings.Join(allowed, ", "))
}
