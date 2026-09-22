// Package testutil builds isolated, fully wired test environments: a fresh
// migrated Postgres database per test, a controllable clock, a fake push
// transport and fixture helpers.
package testutil

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"placementhub/internal/app"
	"placementhub/internal/auth"
	"placementhub/internal/config"
	"placementhub/internal/db"
	"placementhub/internal/push"
	"placementhub/internal/storage"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Password = "Passw0rd!test"

func init() {
	auth.BcryptCost = 4 // fast hashing in tests
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Clock is real time plus a manually advanced offset. Tracking real time keeps it
// consistent with values the database stamps itself (now()), while Advance lets
// tests jump forward past deadlines and expiries.
type Clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

type Env struct {
	T      *testing.T
	Ctx    context.Context
	Pool   *pgxpool.Pool
	App    *app.App
	Cfg    config.Config
	Clock  *Clock
	Sender *push.Fake
	Store  storage.Storage
}

func adminURL() string {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://placementhub:placementhub@localhost:5433/postgres?sslmode=disable"
}

// NewEnv creates an isolated environment. Tests are skipped when Postgres is
// unreachable, unless REQUIRE_DB=1 (use that in CI).
func NewEnv(t *testing.T) *Env {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL())
	if err == nil {
		err = admin.Ping(ctx)
	}
	if err != nil {
		if os.Getenv("REQUIRE_DB") == "1" {
			t.Fatalf("postgres unreachable: %v", err)
		}
		t.Skipf("postgres unreachable (start it with `docker compose up -d db`): %v", err)
	}

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "ph_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	u, _ := url.Parse(adminURL())
	u.Path = "/" + name
	dsn := u.String()

	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})

	loc, _ := time.LoadLocation("Asia/Kolkata")
	cfg := config.Config{
		Env: "test", JWTSecret: []byte("test-secret-test-secret-test-secret-0123"),
		AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour,
		CORSOrigins: []string{"http://localhost:3000"}, Location: loc,
		OfferValidity: 7 * 24 * time.Hour, MaxUploadMB: 5,
		CookieSameSite: http.SameSiteLaxMode, RunWorkers: false,
		VAPIDPublic: "test-public", VAPIDPrivate: "test-private", VAPIDSubject: "mailto:test@example.edu",
	}
	clock := &Clock{}
	sender := &push.Fake{}
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfg, pool, app.Options{Now: clock.Now, Sender: sender, Storage: store})
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	return &Env{T: t, Ctx: ctx, Pool: pool, App: a, Cfg: cfg, Clock: clock, Sender: sender, Store: store}
}

// ---- users ----------------------------------------------------------------

type User struct {
	ID        uuid.UUID
	StudentID uuid.UUID // students
	CompanyID uuid.UUID // companies
	Email     string
	Token     string
}

func (e *Env) mustExec(sql string, args ...any) {
	e.T.Helper()
	if _, err := e.Pool.Exec(e.Ctx, sql, args...); err != nil {
		e.T.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *Env) newUser(email, name, role string) *User {
	e.T.Helper()
	hash, err := auth.HashPassword(Password)
	if err != nil {
		e.T.Fatal(err)
	}
	u := &User{Email: email}
	if err := e.Pool.QueryRow(e.Ctx,
		`INSERT INTO users (email, password_hash, role, name) VALUES ($1, $2, $3, $4) RETURNING id`,
		email, hash, role, name).Scan(&u.ID); err != nil {
		e.T.Fatalf("insert user: %v", err)
	}
	return u
}

func (e *Env) login(u *User) {
	e.T.Helper()
	sess, err := e.App.Auth.Login(e.Ctx, u.Email, Password, "test", "127.0.0.1")
	if err != nil {
		e.T.Fatalf("login %s: %v", u.Email, err)
	}
	u.Token = sess.AccessToken
}

func (e *Env) Admin() *User {
	e.T.Helper()
	u := e.newUser("admin@test.edu", "Dr. Admin", "admin")
	e.login(u)
	return u
}

type StudentOpts struct {
	Name     string
	Roll     string
	Branch   string
	CGPA     float64
	Backlogs int
	NoResume bool
}

// Student creates a student with a resume unless NoResume is set.
func (e *Env) Student(o StudentOpts) *User {
	e.T.Helper()
	if o.Branch == "" {
		o.Branch = "CSE"
	}
	if o.CGPA == 0 {
		o.CGPA = 8.0
	}
	if o.Name == "" {
		o.Name = "Student " + o.Roll
	}
	if o.Roll == "" {
		o.Roll = "R" + uuid.NewString()[:6]
	}
	u := e.newUser(strings.ToLower(o.Roll)+"@college.edu", o.Name, "student")
	if err := e.Pool.QueryRow(e.Ctx,
		`INSERT INTO students (user_id, roll, branch, cgpa, backlogs) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		u.ID, o.Roll, o.Branch, o.CGPA, o.Backlogs).Scan(&u.StudentID); err != nil {
		e.T.Fatalf("insert student: %v", err)
	}
	if !o.NoResume {
		e.mustExec(`INSERT INTO resumes (student_id, storage_key, filename, content_type, size_bytes)
		            VALUES ($1, $2, 'resume.pdf', 'application/pdf', 1000)`, u.StudentID, uuid.NewString()+".pdf")
	}
	e.login(u)
	return u
}

func (e *Env) Company(name string, approved bool) *User {
	e.T.Helper()
	u := e.newUser(strings.ToLower(strings.ReplaceAll(name, " ", ""))+"@corp.test", "HR "+name, "company")
	status := "Pending"
	if approved {
		status = "Approved"
	}
	if err := e.Pool.QueryRow(e.Ctx,
		`INSERT INTO companies (user_id, name, industry, hr_name, email, status) VALUES ($1, $2, 'Tech', 'HR', $3, $4) RETURNING id`,
		u.ID, name, u.Email, status).Scan(&u.CompanyID); err != nil {
		e.T.Fatalf("insert company: %v", err)
	}
	e.login(u)
	return u
}

type JobOpts struct {
	Role          string
	CTC           float64
	MinCGPA       float64
	Branches      []string
	AllowBacklogs bool
	DeadlineDays  int    // days from the clock's today; default 10
	Status        string // default Open
}

// Job inserts a job directly. Open jobs are stamped as approved 5 days ago so
// reminder windows behave as if the job had been live for a while.
func (e *Env) Job(companyID uuid.UUID, o JobOpts) uuid.UUID {
	e.T.Helper()
	if o.Role == "" {
		o.Role = "Software Engineer"
	}
	if o.CTC == 0 {
		o.CTC = 12
	}
	if o.Branches == nil {
		o.Branches = []string{"CSE", "IT", "ECE"}
	}
	if o.DeadlineDays == 0 {
		o.DeadlineDays = 10
	}
	if o.Status == "" {
		o.Status = "Open"
	}
	deadline := e.Clock.Now().In(e.Cfg.Location).AddDate(0, 0, o.DeadlineDays).Format("2006-01-02")
	var id uuid.UUID
	if err := e.Pool.QueryRow(e.Ctx,
		`INSERT INTO jobs (company_id, role, type, location, ctc, min_cgpa, branches, skills, deadline, openings, description,
		                   allow_backlogs, status, approved_at)
		 VALUES ($1, $2, 'Full-time', 'Bengaluru', $3, $4, $5, '{}', $6::date, 5, 'A long enough description.', $7, $8,
		         CASE WHEN $8 = 'Open' THEN now() - interval '5 days' END) RETURNING id`,
		companyID, o.Role, o.CTC, o.MinCGPA, o.Branches, deadline, o.AllowBacklogs, o.Status).Scan(&id); err != nil {
		e.T.Fatalf("insert job: %v", err)
	}
	return id
}

// ---- HTTP -----------------------------------------------------------------

type Resp struct {
	Status int
	Body   []byte
	Header http.Header
	t      *testing.T
}

func (r Resp) JSON() map[string]any {
	r.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		r.t.Fatalf("response is not a JSON object (%d): %s", r.Status, r.Body)
	}
	return m
}

func (r Resp) Decode(v any) {
	r.t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		r.t.Fatalf("decode %s: %v", r.Body, err)
	}
}

// ErrCode returns error.code from an error response.
func (r Resp) ErrCode() string {
	r.t.Helper()
	m := r.JSON()
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

// Items returns the "items" array of a list response.
func (r Resp) Items() []map[string]any {
	r.t.Helper()
	m := r.JSON()
	raw, _ := m["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		out = append(out, x.(map[string]any))
	}
	return out
}

func (e *Env) do(req *http.Request) Resp {
	rec := httptest.NewRecorder()
	e.App.Handler.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return Resp{Status: rec.Code, Body: body, Header: rec.Header(), t: e.T}
}

// Req sends a JSON request; body may be nil, a string of JSON, or any value to marshal.
func (e *Env) Req(u *User, method, path string, body any) Resp {
	e.T.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.T.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if u != nil && u.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.Token)
	}
	return e.do(req)
}

func (e *Env) Get(u *User, path string) Resp { e.T.Helper(); return e.Req(u, "GET", path, nil) }
func (e *Env) Post(u *User, path string, body any) Resp {
	e.T.Helper()
	return e.Req(u, "POST", path, body)
}

// Upload sends a multipart file upload.
func (e *Env) Upload(u *User, path, field, filename string, data []byte) Resp {
	e.T.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile(field, filename)
	_, _ = fw.Write(data)
	_ = mw.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if u != nil {
		req.Header.Set("Authorization", "Bearer "+u.Token)
	}
	return e.do(req)
}

// ---- helpers --------------------------------------------------------------

// Count runs a SELECT count(*) style query.
func (e *Env) Count(sql string, args ...any) int {
	e.T.Helper()
	var n int
	if err := e.Pool.QueryRow(e.Ctx, sql, args...).Scan(&n); err != nil {
		e.T.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// Subscribe registers a fake browser push subscription for a student.
func (e *Env) Subscribe(u *User, id string) string {
	e.T.Helper()
	endpoint := "https://fcm.googleapis.com/fcm/send/" + id
	r := e.Req(u, "POST", "/me/push-subscriptions", map[string]any{
		"endpoint": endpoint,
		"keys": map[string]string{
			"p256dh": "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM",
			"auth":   "tBHItJI5svbpez7KI4CCXg",
		},
	})
	if r.Status != http.StatusCreated {
		e.T.Fatalf("subscribe: %d %s", r.Status, r.Body)
	}
	return endpoint
}

// PDF returns bytes that pass resume validation.
func PDF() []byte {
	return []byte("%PDF-1.4\n" + strings.Repeat("1 0 obj\n<< /Type /Catalog >>\nendobj\n", 10) + "%%EOF\n")
}

func Sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

// Relogin issues a fresh access token, for tests that advance the clock past a token's lifetime.
func (e *Env) Relogin(u *User) { e.T.Helper(); e.login(u) }

// MustExec runs a statement that must succeed (test setup shortcuts).
func (e *Env) MustExec(sql string, args ...any) { e.T.Helper(); e.mustExec(sql, args...) }
