package app_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"placementhub/internal/testutil"
)

func TestSecurityHeadersAndJSONErrorsOnEveryResponse(t *testing.T) {
	w := newWorld(t)
	for _, path := range []string{"/healthz", "/jobs", "/no-such-route", "/auth/login"} {
		rec := raw(w.e, "GET", path, "", nil)
		h := rec.Header()
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" ||
			!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: missing security headers: %v", path, h)
		}
		if h.Get("X-Request-ID") == "" {
			t.Errorf("%s: no request id", path)
		}
	}
	// Unknown routes and wrong methods answer with the standard JSON error, never a stack trace or HTML.
	if rec := raw(w.e, "GET", "/no-such-route", "", nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Errorf("404 body: %d %s", rec.Code, rec.Body)
	}
	if rec := raw(w.e, "DELETE", "/healthz", "", nil); rec.Code != 405 || !strings.Contains(rec.Body.String(), "method_not_allowed") {
		t.Errorf("405 body: %d %s", rec.Code, rec.Body)
	}
	if rec := raw(w.e, "GET", "/readyz", "", nil); rec.Code != 200 {
		t.Errorf("readyz: %d", rec.Code)
	}
}

func TestCORSAllowsOnlyConfiguredOrigins(t *testing.T) {
	w := newWorld(t)
	rec := raw(w.e, "OPTIONS", "/jobs", "", map[string]string{
		"Origin": "http://localhost:3000", "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "authorization",
	})
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" ||
		rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("preflight from the frontend: %d %v", rec.Code, rec.Header())
	}
	rec = raw(w.e, "OPTIONS", "/jobs", "", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "GET"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("foreign origin allowed: %v", rec.Header())
	}
	// A wildcard must never be sent alongside credentials.
	rec = raw(w.e, "GET", "/healthz", "", map[string]string{"Origin": "http://localhost:3000"})
	if rec.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Error("wildcard CORS origin")
	}
}

func TestCookieAuthenticatedEndpointsRejectForeignOrigins(t *testing.T) {
	w := newWorld(t)
	s := w.e.Student(testutil.StudentOpts{Roll: "21CS095"})
	c := refreshCookie(loginHTTP(w.e, s.Email, testutil.Password))

	// A cross-site page riding the victim's cookie is refused (CSRF defence in depth on top of SameSite).
	if rec := raw(w.e, "POST", "/auth/refresh", "", map[string]string{"Origin": "https://evil.example"}, c); rec.Code != 403 {
		t.Errorf("refresh from a foreign origin: %d, want 403", rec.Code)
	}
	if rec := raw(w.e, "POST", "/auth/logout", "", map[string]string{"Origin": "https://evil.example"}, c); rec.Code != 403 {
		t.Errorf("logout from a foreign origin: %d, want 403", rec.Code)
	}
	// The real frontend, and non-browser clients that send no Origin, work.
	if rec := raw(w.e, "POST", "/auth/refresh", "", map[string]string{"Origin": "http://localhost:3000"}, c); rec.Code != 200 {
		t.Errorf("refresh from the frontend: %d", rec.Code)
	}
	// Bearer-token API calls do not depend on cookies at all.
	if r := w.e.Get(&testutil.User{}, "/me"); r.Status != 401 {
		t.Errorf("cookie-less call without a token: %d", r.Status)
	}
}

func TestInjectionAttemptsAreInert(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS096"})
	e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Backend Engineer"})

	payloads := []string{
		"'; DROP TABLE users; --", "' OR '1'='1", `" OR ""="`, "%' OR 1=1 --", "\\'; SELECT pg_sleep(5); --", "\x00", "🔥%00",
	}
	for _, p := range payloads {
		for _, path := range []string{"/jobs?q=", "/jobs?type=", "/jobs?sort="} {
			r := e.Get(s, path+url.QueryEscape(p))
			if r.Status >= 500 {
				t.Errorf("%s%q -> %d %s", path, p, r.Status, r.Body)
			}
		}
		if r := e.Get(w.admin, "/admin/students?q="+url.QueryEscape(p)); r.Status >= 500 {
			t.Errorf("admin search %q -> %d", p, r.Status)
		}
	}
	if n := e.Count(`SELECT count(*) FROM users`); n < 3 {
		t.Fatalf("users table damaged: %d rows", n)
	}
	// A search for the payload text matches nothing rather than everything.
	if got := e.Get(s, "/jobs?q="+url.QueryEscape("' OR '1'='1")).Items(); len(got) != 0 {
		t.Errorf("injection payload matched %d jobs", len(got))
	}
	// Stored HTML is returned as data (JSON), never interpreted by the API.
	e.Req(s, "PUT", "/me/profile", map[string]any{"about": `<script>alert(1)</script>`})
	r := e.Get(s, "/me/profile")
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Errorf("content type = %q", r.Header.Get("Content-Type"))
	}
}

func TestRequestBodyLimitsAndMalformedInput(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS097"})

	huge := `{"about":"` + strings.Repeat("a", 2<<20) + `"}`
	if r := e.Req(s, "PUT", "/me/profile", huge); r.Status != http.StatusRequestEntityTooLarge {
		t.Errorf("2 MB JSON body: %d, want 413", r.Status)
	}
	for name, body := range map[string]string{
		"not json":   `hello`,
		"trailing":   `{"about":"x"} {"about":"y"}`,
		"wrong type": `{"tenth":"ninety"}`,
		"truncated":  `{"about":`,
		"empty":      ``,
		"NUL byte":   `{"about":"a\u0000b"}`,
	} {
		if r := e.Req(s, "PUT", "/me/profile", body); r.Status != 400 {
			t.Errorf("%s: %d %s, want 400", name, r.Status, r.Body)
		}
	}
	// Errors never leak internals.
	r := e.Req(s, "PUT", "/me/profile", `{"tenth":"ninety"}`)
	if strings.Contains(string(r.Body), "json:") || strings.Contains(string(r.Body), "cannot unmarshal") {
		t.Errorf("decoder details leaked: %s", r.Body)
	}
}

func TestServerErrorsDoNotLeakInternals(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS098"})
	// Break the database underneath the API and make sure the response is generic.
	e.MustExec(`ALTER TABLE resumes RENAME TO resumes_gone`)
	r := e.Get(s, "/me/profile")
	if r.Status != 500 {
		t.Fatalf("expected a 500, got %d %s", r.Status, r.Body)
	}
	if strings.Contains(string(r.Body), "resumes") || strings.Contains(string(r.Body), "SQLSTATE") || strings.Contains(string(r.Body), "pgx") {
		t.Errorf("500 response leaks internals: %s", r.Body)
	}
	if r.ErrCode() != "internal" {
		t.Errorf("error code = %q", r.ErrCode())
	}
	e.MustExec(`ALTER TABLE resumes_gone RENAME TO resumes`)
}

func TestSensitiveActionsAreAudited(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS099"})
	o := w.offer(w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{})))
	w.accept(s, o)
	e.Post(w.admin, "/admin/offers/"+o+"/revoke", map[string]any{"reason": "Company withdrew"})
	e.Post(w.admin, "/admin/students/"+s.StudentID.String()+"/deactivate", nil)
	loginHTTP(e, "someone@nowhere.edu", "x")
	for _, action := range []string{"offer.revoked", "student.active_changed"} {
		if n := e.Count(`SELECT count(*) FROM audit_log WHERE action = $1`, action); n != 1 {
			t.Errorf("audit rows for %s = %d, want 1", action, n)
		}
	}
	if n := e.Count(`SELECT count(*) FROM audit_log WHERE actor_user_id = $1`, w.admin.ID); n < 2 {
		t.Errorf("admin actions not attributed to the admin")
	}
}
