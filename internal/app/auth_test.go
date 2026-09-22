package app_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"placementhub/internal/testutil"

	"github.com/golang-jwt/jwt/v5"
)

// raw sends a request with arbitrary headers and cookies.
func raw(e *testutil.Env, method, path, body string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.App.Handler.ServeHTTP(rec, req)
	return rec
}

func refreshCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "ph_refresh" {
			return c
		}
	}
	return nil
}

func loginHTTP(e *testutil.Env, email, pw string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"email": email, "password": pw})
	return raw(e, "POST", "/auth/login", string(b), nil)
}

func TestLoginRefreshRotationAndReuseDetection(t *testing.T) {
	w := newWorld(t)
	s := w.e.Student(testutil.StudentOpts{Roll: "21CS010"})

	rec := loginHTTP(w.e, s.Email, testutil.Password)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	c1 := refreshCookie(rec)
	if c1 == nil || !c1.HttpOnly || c1.Path != "/auth" || c1.SameSite != http.SameSiteLaxMode {
		t.Fatalf("refresh cookie missing or unsafe: %+v", c1)
	}
	var login struct {
		AccessToken string `json:"accessToken"`
		User        struct {
			Role string `json:"role"`
		} `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	if login.User.Role != "student" || login.AccessToken == "" {
		t.Fatalf("login body: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), c1.Value) {
		t.Fatal("the refresh token must never appear in the response body")
	}

	// The access token opens /me.
	me := w.e.Req(&testutil.User{Token: login.AccessToken}, "GET", "/me", nil)
	if me.Status != 200 {
		t.Fatalf("/me: %d %s", me.Status, me.Body)
	}

	// Refresh rotates: a new cookie, and the old one stops working.
	r2 := raw(w.e, "POST", "/auth/refresh", "", nil, c1)
	if r2.Code != 200 {
		t.Fatalf("refresh: %d %s", r2.Code, r2.Body)
	}
	c2 := refreshCookie(r2)
	if c2 == nil || c2.Value == c1.Value {
		t.Fatal("refresh token was not rotated")
	}

	// Replaying the old token is treated as theft: it fails AND kills the family.
	if r := raw(w.e, "POST", "/auth/refresh", "", nil, c1); r.Code != 401 {
		t.Fatalf("replayed refresh token: %d, want 401", r.Code)
	}
	if r := raw(w.e, "POST", "/auth/refresh", "", nil, c2); r.Code != 401 {
		t.Fatalf("legitimate token after reuse detection: %d, want 401 (family revoked)", r.Code)
	}
}

func TestLogoutRevokesRefreshToken(t *testing.T) {
	w := newWorld(t)
	s := w.e.Student(testutil.StudentOpts{Roll: "21CS011"})
	c := refreshCookie(loginHTTP(w.e, s.Email, testutil.Password))

	if r := raw(w.e, "POST", "/auth/logout", "", nil, c); r.Code != 204 {
		t.Fatalf("logout: %d", r.Code)
	}
	if r := raw(w.e, "POST", "/auth/refresh", "", nil, c); r.Code != 401 {
		t.Fatalf("refresh after logout: %d, want 401", r.Code)
	}
	if r := raw(w.e, "POST", "/auth/logout", "", nil); r.Code != 204 {
		t.Fatalf("logout without a cookie should be a harmless 204, got %d", r.Code)
	}
}

func TestLoginFailuresDoNotRevealWhichEmailsExist(t *testing.T) {
	w := newWorld(t)
	s := w.e.Student(testutil.StudentOpts{Roll: "21CS012"})

	wrongPw := loginHTTP(w.e, s.Email, "definitely-wrong")
	unknown := loginHTTP(w.e, "nobody@nowhere.edu", "definitely-wrong")
	if wrongPw.Code != 401 || unknown.Code != 401 {
		t.Fatalf("statuses = %d/%d, want 401/401", wrongPw.Code, unknown.Code)
	}
	if wrongPw.Body.String() != unknown.Body.String() {
		t.Errorf("responses differ:\n%s\n%s", wrongPw.Body, unknown.Body)
	}

	// Deactivated accounts cannot sign in, and lose their sessions.
	c := refreshCookie(loginHTTP(w.e, s.Email, testutil.Password))
	if r := w.e.Post(w.admin, "/admin/students/"+s.StudentID.String()+"/deactivate", nil); r.Status != 204 {
		t.Fatalf("deactivate: %d %s", r.Status, r.Body)
	}
	if r := loginHTTP(w.e, s.Email, testutil.Password); r.Code != 401 {
		t.Errorf("deactivated login: %d, want 401", r.Code)
	}
	if r := raw(w.e, "POST", "/auth/refresh", "", nil, c); r.Code != 401 {
		t.Errorf("deactivated refresh: %d, want 401", r.Code)
	}
}

func TestLoginIsRateLimitedPerAccount(t *testing.T) {
	w := newWorld(t)
	s := w.e.Student(testutil.StudentOpts{Roll: "21CS013"})
	got429 := false
	for i := 0; i < 12; i++ {
		rec := raw(w.e, "POST", "/auth/login", `{"email":"`+s.Email+`","password":"nope"}`, nil)
		if rec.Code == 429 {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("repeated failed logins were never rate limited")
	}
	// Even the right password is refused while limited: brute force gets no oracle.
	if rec := loginHTTP(w.e, s.Email, testutil.Password); rec.Code != 429 {
		t.Errorf("valid login while throttled = %d, want 429", rec.Code)
	}
}

func TestRoleBasedAccessControl(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS014"})

	cases := []struct {
		who    *testutil.User
		method string
		path   string
		want   int
	}{
		{nil, "GET", "/me", 401},
		{nil, "GET", "/jobs", 401},
		{nil, "GET", "/admin/students", 401},
		{s, "GET", "/admin/students", 403},
		{s, "GET", "/admin/reports/summary", 403},
		{s, "POST", "/admin/notices", 403},
		{s, "GET", "/company/jobs", 403},
		{s, "GET", "/company/candidates", 403},
		{w.company, "GET", "/jobs", 403},
		{w.company, "GET", "/me/applications", 403},
		{w.company, "GET", "/me/notifications", 403}, // push/inbox is student-only in V1
		{w.company, "POST", "/me/push-subscriptions", 403},
		{w.company, "GET", "/admin/students", 403},
		{w.admin, "GET", "/jobs", 403},
		{w.admin, "GET", "/company/jobs", 403},
		{w.admin, "GET", "/me/notifications", 403},
		{s, "GET", "/me", 200},
		{w.company, "GET", "/me", 200},
		{w.admin, "GET", "/me", 200},
		{w.admin, "GET", "/admin/students", 200},
		{w.company, "GET", "/company/jobs", 200},
		{s, "GET", "/jobs", 200},
	}
	for _, c := range cases {
		if r := e.Req(c.who, c.method, c.path, nil); r.Status != c.want {
			who := "anonymous"
			if c.who != nil {
				who = c.who.Email
			}
			t.Errorf("%s %s %s = %d, want %d", who, c.method, c.path, r.Status, c.want)
		}
	}
}

func TestForgedAndExpiredTokensAreRejected(t *testing.T) {
	w := newWorld(t)
	s := w.e.Student(testutil.StudentOpts{Roll: "21CS015"})
	secret := w.e.Cfg.JWTSecret

	sign := func(method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
		tok, err := jwt.NewWithClaims(method, claims).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	valid := func(exp time.Time) jwt.MapClaims {
		return jwt.MapClaims{"sub": s.ID.String(), "role": "admin", "iss": "placementhub", "exp": exp.Unix()}
	}

	// alg=none, wrong secret, expired, wrong issuer, garbage.
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"`+s.ID.String()+`","role":"admin","iss":"placementhub","exp":`+
			testutil.Sprintf("%d", time.Now().Add(time.Hour).Unix())+`}`)) + "."
	tokens := map[string]string{
		"alg none":     none,
		"wrong secret": sign(jwt.SigningMethodHS256, []byte("another-secret-another-secret-12345"), valid(time.Now().Add(time.Hour))),
		"expired":      sign(jwt.SigningMethodHS256, secret, valid(time.Now().Add(-time.Hour))),
		"wrong issuer": sign(jwt.SigningMethodHS256, secret, jwt.MapClaims{"sub": s.ID.String(), "role": "admin", "iss": "evil", "exp": time.Now().Add(time.Hour).Unix()}),
		"no expiry":    sign(jwt.SigningMethodHS256, secret, jwt.MapClaims{"sub": s.ID.String(), "role": "admin", "iss": "placementhub"}),
		"garbage":      "not.a.jwt",
	}
	for name, tok := range tokens {
		if r := w.e.Req(&testutil.User{Token: tok}, "GET", "/admin/students", nil); r.Status != 401 {
			t.Errorf("%s token: %d, want 401", name, r.Status)
		}
	}
	// Sanity: a correctly signed token is accepted (and role comes from the signed claim).
	ok := sign(jwt.SigningMethodHS256, secret, valid(time.Now().Add(time.Hour)))
	if r := w.e.Req(&testutil.User{Token: ok}, "GET", "/admin/students", nil); r.Status == 401 {
		t.Errorf("a validly signed token was rejected")
	}
}

func TestTemporaryPasswordMustBeChangedBeforeAnythingElse(t *testing.T) {
	w := newWorld(t)
	e := w.e
	// Admin creates a student: they receive a temporary password once.
	r := e.Post(w.admin, "/admin/students", map[string]any{
		"name": "New Student", "roll": "21cs900", "email": "New.Student@College.edu", "branch": "CSE", "cgpa": 8.2,
	})
	if r.Status != 201 {
		t.Fatalf("create student: %d %s", r.Status, r.Body)
	}
	temp := r.JSON()["tempPassword"].(string)
	if len(temp) < 10 {
		t.Fatalf("temp password too short: %q", temp)
	}

	rec := loginHTTP(e, "new.student@college.edu", temp) // email is case-insensitive
	if rec.Code != 200 {
		t.Fatalf("login with temp password: %d %s", rec.Code, rec.Body)
	}
	var sess struct {
		AccessToken string `json:"accessToken"`
		User        struct {
			MustChange bool `json:"mustChangePassword"`
		} `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	if !sess.User.MustChange {
		t.Fatal("mustChangePassword should be true for a temporary password")
	}
	u := &testutil.User{Token: sess.AccessToken}

	// Everything except /me and change-password is blocked.
	if r := e.Get(u, "/jobs"); r.Status != 403 || r.ErrCode() != "password_change_required" {
		t.Errorf("jobs with temp password: %d %s", r.Status, r.Body)
	}
	if r := e.Get(u, "/me/profile"); r.Status != 403 {
		t.Errorf("profile with temp password: %d", r.Status)
	}
	if r := e.Get(u, "/me"); r.Status != 200 {
		t.Errorf("/me must stay reachable: %d", r.Status)
	}

	// Weak or unchanged passwords are refused.
	for _, bad := range []string{"short", temp, strings.Repeat("a", 73)} {
		if r := e.Post(u, "/auth/change-password", map[string]any{"currentPassword": temp, "newPassword": bad}); r.Status != 422 {
			t.Errorf("new password %q: %d, want 422", bad, r.Status)
		}
	}
	if r := e.Post(u, "/auth/change-password", map[string]any{"currentPassword": "wrong", "newPassword": "A-good-new-passw0rd"}); r.Status != 422 {
		t.Errorf("wrong current password: %d, want 422", r.Status)
	}
	r = e.Post(u, "/auth/change-password", map[string]any{"currentPassword": temp, "newPassword": "A-good-new-passw0rd"})
	if r.Status != 200 {
		t.Fatalf("change password: %d %s", r.Status, r.Body)
	}
	u.Token = r.JSON()["accessToken"].(string)
	if r := e.Get(u, "/me/profile"); r.Status != 200 {
		t.Errorf("profile after changing password: %d %s", r.Status, r.Body)
	}
	if rec := loginHTTP(e, "new.student@college.edu", temp); rec.Code != 401 {
		t.Errorf("old password still works: %d", rec.Code)
	}
}

func TestCompanyRegistrationStartsPendingAndCannotSubmitJobs(t *testing.T) {
	e := testutil.NewEnv(t)
	admin := e.Admin()

	reg := `{"companyName":"Orbit Cloud","industry":"Cloud","hrName":"Neha Bhatt","email":"Neha@Orbit.example","phone":"+91 98765 43210","password":"Str0ng-passw0rd"}`
	if rec := raw(e, "POST", "/auth/register/company", reg, nil); rec.Code != 201 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	if rec := raw(e, "POST", "/auth/register/company", reg, nil); rec.Code != 409 {
		t.Errorf("duplicate registration: %d, want 409", rec.Code)
	}
	if rec := raw(e, "POST", "/auth/register/company", `{"companyName":"x","email":"bad","password":"1"}`, nil); rec.Code != 422 {
		t.Errorf("invalid registration: %d, want 422", rec.Code)
	}
	// Registration must never allow choosing a role.
	evil := `{"companyName":"Evil Corp","hrName":"Eve","email":"eve@evil.example","password":"Str0ng-passw0rd","role":"admin"}`
	raw(e, "POST", "/auth/register/company", evil, nil)
	if n := e.Count(`SELECT count(*) FROM users WHERE email = 'eve@evil.example' AND role = 'company'`); n != 1 {
		t.Errorf("registered user is not a plain company account")
	}

	rec := loginHTTP(e, "neha@orbit.example", "Str0ng-passw0rd")
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var s struct {
		AccessToken string `json:"accessToken"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	co := &testutil.User{Token: s.AccessToken}

	job := map[string]any{
		"role": "SDE Intern", "type": "Internship", "location": "Remote", "ctc": 7.2, "minCgpa": 7.0,
		"branches": []string{"CSE"}, "skills": []string{"Java"}, "openings": 5, "description": "Six-month internship.",
		"deadline": e.Clock.Now().AddDate(0, 0, 20).Format("2006-01-02"),
	}
	created := e.Post(co, "/company/jobs", job)
	if created.Status != 201 || created.JSON()["status"] != "Draft" {
		t.Fatalf("draft: %d %s", created.Status, created.Body)
	}
	id := created.JSON()["id"].(string)
	if r := e.Post(co, "/company/jobs/"+id+"/submit", nil); r.Status != 422 || r.ErrCode() != "company_not_approved" {
		t.Fatalf("submit while pending: %d %s", r.Status, r.Body)
	}

	// Admin approves the company; now it can submit.
	var companyID string
	for _, c := range e.Get(admin, "/admin/companies?status=Pending").Items() {
		companyID = c["id"].(string)
	}
	if r := e.Req(admin, "PATCH", "/admin/companies/"+companyID+"/status", map[string]any{"status": "Approved"}); r.Status != 200 {
		t.Fatalf("approve company: %d %s", r.Status, r.Body)
	}
	if r := e.Post(co, "/company/jobs/"+id+"/submit", nil); r.Status != 200 || r.JSON()["status"] != "Pending" {
		t.Fatalf("submit after approval: %d %s", r.Status, r.Body)
	}
}
