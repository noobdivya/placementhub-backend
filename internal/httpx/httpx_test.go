package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidatorCollectsEveryFieldError(t *testing.T) {
	var v V
	v.Text("name", "", 1, 10)
	v.Text("about", strings.Repeat("x", 11), 0, 10)
	v.Text("nick", "a", 2, 10)
	v.Email("email", "not-an-email")
	v.Range("cgpa", 11, 0, 10)
	v.OneOf("type", "Gig", "Full-time", "Internship")
	err := v.Err()
	he, ok := err.(*Error)
	if !ok || he.Status != 422 || he.Code != "validation_failed" {
		t.Fatalf("err = %#v", err)
	}
	for _, f := range []string{"name", "about", "nick", "email", "cgpa", "type"} {
		if he.Fields[f] == "" {
			t.Errorf("no message for %q: %v", f, he.Fields)
		}
	}
	if (&V{}).Err() != nil {
		t.Error("an empty validator must be valid")
	}
}

func TestEmailValidation(t *testing.T) {
	for _, ok := range []string{"a@b.co", "first.last@college.edu", "x+tag@sub.domain.org"} {
		var v V
		v.Email("e", ok)
		if v.Err() != nil {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "a", "a@b", "Name <a@b.co>", "a@b.co, c@d.co", "a b@c.de", strings.Repeat("a", 250) + "@b.co"} {
		var v V
		v.Email("e", bad)
		if v.Err() == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTextCountsRunesAndTrims(t *testing.T) {
	var v V
	v.Text("f", "  \t ", 1, 5)
	if v.Err() == nil {
		t.Error("whitespace-only passed a required check")
	}
	v = V{}
	v.Text("f", "日本語です", 1, 5) // five runes, fifteen bytes
	if v.Err() != nil {
		t.Error("multi-byte text was measured in bytes")
	}
}

func TestPaging(t *testing.T) {
	cases := []struct {
		q                   string
		page, limit, offset int
	}{
		{"", 1, 20, 0}, {"page=3&limit=10", 3, 10, 20}, {"limit=5000", 1, 100, 0},
		{"page=0&limit=0", 1, 20, 0}, {"page=-4", 1, 20, 0}, {"page=abc&limit=xyz", 1, 20, 0},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/x?"+c.q, nil)
		p, l, o := Paging(r, 20, 100)
		if p != c.page || l != c.limit || o != c.offset {
			t.Errorf("Paging(%q) = %d,%d,%d want %d,%d,%d", c.q, p, l, o, c.page, c.limit, c.offset)
		}
	}
}

func TestCSVSafeNeutralisesFormulas(t *testing.T) {
	cases := map[string]string{
		"=1+1": "'=1+1", "+cmd": "'+cmd", "-2": "'-2", "@SUM(A1)": "'@SUM(A1)", "\tx": "'\tx", "\rx": "'\rx",
		"plain": "plain", "": "", "a=b": "a=b", "8.6": "8.6",
	}
	for in, want := range cases {
		if got := CSVSafe(in); got != want {
			t.Errorf("CSVSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecode(t *testing.T) {
	type body struct {
		A string `json:"a"`
	}
	try := func(s string) (body, error) {
		var b body
		r := httptest.NewRequest("POST", "/", strings.NewReader(s))
		return b, Decode(httptest.NewRecorder(), r, &b)
	}
	if b, err := try(`{"a":"x","unknown":1}`); err != nil || b.A != "x" {
		t.Errorf("valid body: %v %v", b, err)
	}
	for _, bad := range []string{``, `nope`, `{"a":1}`, `{"a":"x"}{"a":"y"}`, `[1,2]`} {
		if _, err := try(bad); err == nil {
			t.Errorf("%q accepted", bad)
		} else if he := err.(*Error); he.Status != 400 {
			t.Errorf("%q -> %d", bad, he.Status)
		}
	}
	if _, err := try(`{"a":"` + strings.Repeat("x", 2<<20) + `"}`); err == nil || err.(*Error).Status != 413 {
		t.Errorf("oversized body: %v", err)
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	r.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1")
	if got := ClientIP(r, false); got != "203.0.113.9" {
		t.Errorf("untrusted proxy: got %q, a client-supplied header must be ignored", got)
	}
	if got := ClientIP(r, true); got != "198.51.100.7" {
		t.Errorf("trusted proxy: got %q", got)
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(60, 3)
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("request %d denied within the burst", i)
		}
	}
	if l.Allow("a") {
		t.Error("burst exceeded but request allowed")
	}
	if !l.Allow("b") {
		t.Error("keys must be independent")
	}
	h := l.Limit(false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	got429 := false
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.0.2.1:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == 429 {
			got429 = true
			if rec.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
			var e struct{ Error Error }
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
		}
	}
	if !got429 {
		t.Error("middleware never limited")
	}
}

func TestRecoverTurnsPanicsInto500WithoutLeaking(t *testing.T) {
	h := Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("secret internal detail") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestWriteErrorHidesUnknownErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest("GET", "/", nil), errString("connection to db-prod-3.internal failed"))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "db-prod") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest("GET", "/", nil), Forbidden("nope"))
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), `"code":"forbidden"`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
