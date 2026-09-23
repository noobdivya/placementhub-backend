package app_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"placementhub/internal/email"
	"placementhub/internal/testutil"

	"github.com/google/uuid"
)

// emailFor returns the most recently queued email for one of a user's
// notifications of the given type.
func emailFor(t *testing.T, e *testutil.Env, u *testutil.User, notifType string) (subject, html, text, toEmail string) {
	t.Helper()
	err := e.Pool.QueryRow(e.Ctx, `
		SELECT eo.subject, eo.body_html, eo.body_text, eo.to_email
		  FROM email_outbox eo JOIN notifications n ON n.id = eo.notification_id
		 WHERE n.user_id = $1 AND n.type = $2
		 ORDER BY eo.id DESC LIMIT 1`, u.ID, notifType).Scan(&subject, &html, &text, &toEmail)
	if err != nil {
		t.Fatalf("email for %s: %v", notifType, err)
	}
	return
}

func emailCount(e *testutil.Env, u *testutil.User, notifType string) int {
	return e.Count(`SELECT count(*) FROM email_outbox eo JOIN notifications n ON n.id = eo.notification_id WHERE n.user_id = $1 AND n.type = $2`, u.ID, notifType)
}

// TestApplicationStageEmails walks one application through every stage that
// queues an email, and checks each one's recipient/subject/content.
func TestApplicationStageEmails(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "31CS001"})

	id := w.postAndApproveJob(map[string]any{"role": "SDE Intern", "minCgpa": 0})
	w.approve(id)
	appID := w.mustApply(s, uuid.MustParse(id))

	w.mustMove(appID, "Shortlisted")
	subj, html, text, to := emailFor(t, e, s, "application_update")
	if subj != "Shortlisted – Nimbus Labs" {
		t.Errorf("shortlist subject = %q", subj)
	}
	if to != s.Email {
		t.Errorf("shortlist to = %q, want %q", to, s.Email)
	}
	if !strings.Contains(html, "SDE Intern") || !strings.Contains(text, "SDE Intern") {
		t.Errorf("shortlist email missing role:\nhtml=%s\ntext=%s", html, text)
	}

	w.mustMove(appID, "Interview")
	subj, _, _, _ = emailFor(t, e, s, "interview")
	if subj != "Interview Stage – Nimbus Labs" {
		t.Errorf("interview subject = %q", subj)
	}

	c := w.mustMove(appID, "Offered")
	subj, html, text, _ = emailFor(t, e, s, "offer")
	if subj != "Offer Extended – Nimbus Labs" {
		t.Errorf("offer subject = %q", subj)
	}
	if !strings.Contains(html, "16.0") || !strings.Contains(html, "LPA") {
		t.Errorf("offer email missing CTC:\n%s", html)
	}
	offerID := c["offer"].(map[string]any)["id"].(string)

	if r := w.accept(s, offerID); r.Status != 200 {
		t.Fatalf("accept: %d %s", r.Status, r.Body)
	}
	subj, _, _, _ = emailFor(t, e, s, "placement")
	if subj != "You're Placed at Nimbus Labs!" {
		t.Errorf("placement subject = %q", subj)
	}
}

func TestApplicationRejectedEmail(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "31CS002"})
	id := w.postAndApproveJob(map[string]any{"minCgpa": 0})
	w.approve(id)
	appID := w.mustApply(s, uuid.MustParse(id))
	w.mustMove(appID, "Rejected")
	subj, _, _, _ := emailFor(t, e, s, "application_update")
	if subj != "Application Update – Nimbus Labs" {
		t.Errorf("rejection subject = %q", subj)
	}
}

func TestRoundEmails(t *testing.T) {
	w := newWorld(t)
	e := w.e
	id := w.postJobWithRound("Aptitude Test")
	// Add a second round so Clear can report a "next round".
	r := e.Req(w.company, "PUT", "/company/jobs/"+id+"/rounds", map[string]any{
		"items": []map[string]any{
			{"name": "Aptitude Test", "mode": "Online", "durationMinutes": 60},
			{"name": "Technical Interview", "mode": "Offline", "durationMinutes": 45},
		},
	})
	if r.Status != 200 {
		t.Fatalf("define rounds: %d %s", r.Status, r.Body)
	}
	e.Post(w.company, "/company/jobs/"+id+"/submit", nil)
	w.approve(id)

	s := e.Student(testutil.StudentOpts{Roll: "31CS003"})
	appID := w.mustApply(s, uuid.MustParse(id))
	rounds := e.Get(w.company, "/company/jobs/"+id+"/rounds").Items()
	roundID := rounds[0]["id"].(string)

	// Give the round a real schedule before the recruiter marks it Scheduled
	// for the candidate, so the email's "When" field is populated.
	when := e.Clock.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if r := e.Req(w.company, "PUT", "/company/jobs/"+id+"/rounds", map[string]any{"items": []map[string]any{
		{"id": roundID, "name": "Aptitude Test", "mode": "Online", "durationMinutes": 60, "scheduledAt": when, "location": "https://meet.example.com/x", "instructions": "Bring your laptop"},
		{"name": "Technical Interview", "mode": "Offline", "durationMinutes": 45},
	}}); r.Status != 200 {
		t.Fatalf("schedule round logistics: %d %s", r.Status, r.Body)
	}

	if r := e.Req(w.company, "PATCH", "/company/applications/"+appID+"/rounds/"+roundID, map[string]any{"status": "Scheduled"}); r.Status != 200 {
		t.Fatalf("set Scheduled: %d %s", r.Status, r.Body)
	}
	subj, html, text, _ := emailFor(t, e, s, "round_scheduled")
	if subj != "Aptitude Test Scheduled – Nimbus Labs" {
		t.Errorf("round_scheduled subject = %q", subj)
	}
	for _, want := range []string{"Meeting link", "https://meet.example.com/x", "Bring your laptop", "60 minutes"} {
		if !strings.Contains(html, want) {
			t.Errorf("round_scheduled html missing %q:\n%s", want, html)
		}
	}
	if !strings.Contains(text, "https://meet.example.com/x") {
		t.Errorf("round_scheduled text missing location:\n%s", text)
	}

	// Rescheduling (PUT with different logistics on an already-locked round)
	// notifies every candidate tracked against it as round_updated.
	when2 := e.Clock.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	if r := e.Req(w.company, "PUT", "/company/jobs/"+id+"/rounds", map[string]any{"items": []map[string]any{
		{"id": roundID, "name": "Aptitude Test", "mode": "Online", "durationMinutes": 60, "scheduledAt": when2, "location": "https://meet.example.com/y"},
		{"name": "Technical Interview", "mode": "Offline", "durationMinutes": 45},
	}}); r.Status != 200 {
		t.Fatalf("reschedule: %d %s", r.Status, r.Body)
	}
	subj, html, _, _ = emailFor(t, e, s, "round_updated")
	if subj != "Aptitude Test Rescheduled – Nimbus Labs" {
		t.Errorf("round_updated subject = %q", subj)
	}
	if !strings.Contains(html, "https://meet.example.com/y") {
		t.Errorf("round_updated html missing new location:\n%s", html)
	}

	if r := e.Req(w.company, "PATCH", "/company/applications/"+appID+"/rounds/"+roundID, map[string]any{"status": "Cleared"}); r.Status != 200 {
		t.Fatalf("clear: %d %s", r.Status, r.Body)
	}
	subj, html, _, _ = emailFor(t, e, s, "round_cleared")
	if subj != "Aptitude Test Cleared – Nimbus Labs" {
		t.Errorf("round_cleared subject = %q", subj)
	}
	if !strings.Contains(html, "Technical Interview") {
		t.Errorf("round_cleared html missing next round name:\n%s", html)
	}

	// A second candidate, rejected straight out of Scheduled.
	other := e.Student(testutil.StudentOpts{Roll: "31CS004"})
	otherApp := w.mustApply(other, uuid.MustParse(id))
	if r := e.Req(w.company, "PATCH", "/company/applications/"+otherApp+"/rounds/"+roundID, map[string]any{"status": "Scheduled"}); r.Status != 200 {
		t.Fatalf("schedule other: %d %s", r.Status, r.Body)
	}
	if r := e.Req(w.company, "PATCH", "/company/applications/"+otherApp+"/rounds/"+roundID, map[string]any{"status": "Rejected"}); r.Status != 200 {
		t.Fatalf("reject other: %d %s", r.Status, r.Body)
	}
	subj, _, _, _ = emailFor(t, e, other, "round_rejected")
	if subj != "Update on Aptitude Test – Nimbus Labs" {
		t.Errorf("round_rejected subject = %q", subj)
	}
}

// TestEmailMuteRulesMirrorPush confirms email obeys the same per-category and
// master-switch rules push does: application_update is mutable, interviews
// and offers are critical and only the master switch silences them.
func TestEmailMuteRulesMirrorPush(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "31CS005"})

	a1 := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{Role: "One"}))
	a2 := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Two"}))
	a3 := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Three"}))

	w.mustMove(a1, "Shortlisted")
	if n := emailCount(e, s, "application_update"); n != 1 {
		t.Fatalf("shortlist email queued = %d, want 1", n)
	}

	// Mute application-update email only (push stays on).
	if r := e.Req(s, "PUT", "/me/notification-preferences", map[string]any{"emailCategories": map[string]bool{"application_update": false}}); r.Status != 200 {
		t.Fatalf("mute email category: %d %s", r.Status, r.Body)
	}
	w.mustMove(a2, "Shortlisted")
	if n := emailCount(e, s, "application_update"); n != 1 {
		t.Errorf("email queued despite muted category (%d)", n)
	}
	// Inbox entry still exists regardless of the email mute.
	if n := e.Count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'application_update'`, s.ID); n != 2 {
		t.Errorf("inbox rows = %d, want 2 (muting email never hides the inbox entry)", n)
	}

	// Interview is critical: ignores the category mute.
	w.mustMove(a1, "Interview")
	if n := emailCount(e, s, "interview"); n != 1 {
		t.Errorf("interview email not queued despite category mute (%d)", n)
	}

	// Only the master email switch silences critical mail.
	if r := e.Req(s, "PUT", "/me/notification-preferences", map[string]any{"emailEnabled": false}); r.Status != 200 {
		t.Fatalf("disable email: %d %s", r.Status, r.Body)
	}
	w.mustMove(a2, "Interview")
	w.mustMove(a3, "Shortlisted")
	if n := emailCount(e, s, "interview"); n != 1 {
		t.Errorf("interview email queued while master switch is off (%d)", n)
	}
	// a1's original shortlist is the only application_update email queued so
	// far (a2's was already category-muted before the master switch flipped);
	// a3's first Shortlisted move must not add a second one while it's off.
	if n := emailCount(e, s, "application_update"); n != 1 {
		t.Errorf("shortlist email queued while master switch is off (%d)", n)
	}

	// Re-enabling resumes email for new events.
	if r := e.Req(s, "PUT", "/me/notification-preferences", map[string]any{"emailEnabled": true, "emailCategories": map[string]bool{"application_update": true}}); r.Status != 200 {
		t.Fatalf("re-enable: %d %s", r.Status, r.Body)
	}
	w.mustMove(a3, "Rejected")
	if n := emailCount(e, s, "application_update"); n != 2 {
		t.Errorf("email not resumed after re-enabling (%d)", n)
	}
}

func TestNotificationPreferencesAPIReportsEmailFields(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "31CS006"})
	p := e.Get(s, "/me/notification-preferences").JSON()
	if p["emailAvailable"] != true || p["emailEnabled"] != true {
		t.Fatalf("defaults = %v", p)
	}
	cats := p["categories"].([]any)
	first := cats[0].(map[string]any)
	if _, ok := first["email"]; !ok {
		t.Errorf("category missing email field: %v", first)
	}
}

// ---- worker delivery outcomes ----------------------------------------------

// newEmailOutbox queues an email for a fresh notification directly, mirroring
// notifications_test.go's newOutbox for push.
func newEmailOutbox(t *testing.T, e *testutil.Env, u *testutil.User, subject string) (outboxID int64) {
	t.Helper()
	var nid uuid.UUID
	if err := e.Pool.QueryRow(e.Ctx,
		`INSERT INTO notifications (user_id, type, category, title, body, link, dedupe_key)
		 VALUES ($1, 'notice', 'notice', 'T', 'B', '/x', $2) RETURNING id`,
		u.ID, uuid.NewString()).Scan(&nid); err != nil {
		t.Fatal(err)
	}
	if err := e.Pool.QueryRow(e.Ctx,
		`INSERT INTO email_outbox (notification_id, to_email, to_name, subject, body_html, body_text)
		 VALUES ($1, $2, $3, $4, '<p>hi</p>', 'hi') RETURNING id`,
		nid, u.Email, "Test User", subject).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	return outboxID
}

func emailOutboxRow(t *testing.T, e *testutil.Env, id int64) (status string, attempts int, next time.Time, lastErr string) {
	t.Helper()
	if err := e.Pool.QueryRow(e.Ctx, `SELECT status, attempts, next_attempt_at, last_error FROM email_outbox WHERE id = $1`, id).
		Scan(&status, &attempts, &next, &lastErr); err != nil {
		t.Fatal(err)
	}
	return
}

func TestEmailWorkerDeliveryOutcomes(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "31CS007"})
	process := func() int {
		n, err := e.App.Email.ProcessOnce(e.Ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("success", func(t *testing.T) {
		id := newEmailOutbox(t, e, s, "Success case")
		e.EmailSender.Status = nil
		process()
		if st, _, _, _ := emailOutboxRow(t, e, id); st != "sent" {
			t.Errorf("status = %s, want sent", st)
		}
	})

	t.Run("server error is retried with backoff then delivered", func(t *testing.T) {
		id := newEmailOutbox(t, e, s, "Retry case")
		e.EmailSender.Status = func(email.Message) (int, string, error) { return 503, "", nil }
		process()
		st, attempts, next, lastErr := emailOutboxRow(t, e, id)
		if st != "pending" || attempts != 1 || !strings.Contains(lastErr, "503") {
			t.Fatalf("after failure: %s attempts=%d err=%q", st, attempts, lastErr)
		}
		if !next.After(e.Clock.Now().Add(20 * time.Second)) {
			t.Errorf("no backoff: next attempt at %v (now %v)", next, e.Clock.Now())
		}
		if n := process(); n != 0 {
			t.Errorf("retried before the backoff elapsed (%d)", n)
		}
		e.Clock.Advance(2 * time.Minute)
		e.EmailSender.Status = nil
		process()
		if st, attempts, _, _ := emailOutboxRow(t, e, id); st != "sent" || attempts != 2 {
			t.Errorf("after retry: %s attempts=%d", st, attempts)
		}
	})

	t.Run("network error is retried", func(t *testing.T) {
		id := newEmailOutbox(t, e, s, "Network case")
		e.EmailSender.Status = func(email.Message) (int, string, error) { return 0, "", errors.New("connection reset") }
		process()
		if st, _, _, msg := emailOutboxRow(t, e, id); st != "pending" || !strings.Contains(msg, "connection reset") {
			t.Errorf("status=%s err=%q", st, msg)
		}
		e.Clock.Advance(time.Hour)
		e.EmailSender.Status = nil
		process()
	})

	t.Run("gives up after repeated failures", func(t *testing.T) {
		id := newEmailOutbox(t, e, s, "Give up case")
		e.EmailSender.Status = func(email.Message) (int, string, error) { return 500, "", nil }
		for i := 0; i < 8; i++ {
			process()
			e.Clock.Advance(2 * time.Hour)
		}
		if st, attempts, _, _ := emailOutboxRow(t, e, id); st != "failed" || attempts != 6 {
			t.Errorf("status=%s attempts=%d, want failed after 6", st, attempts)
		}
		e.EmailSender.Status = nil
	})

	t.Run("permanent rejection is not retried", func(t *testing.T) {
		id := newEmailOutbox(t, e, s, "Permanent case")
		e.EmailSender.Status = func(email.Message) (int, string, error) { return 422, "", nil }
		process()
		if st, attempts, _, _ := emailOutboxRow(t, e, id); st != "failed" || attempts != 1 {
			t.Errorf("status=%s attempts=%d, want failed after 1", st, attempts)
		}
		e.EmailSender.Status = nil
	})
}

func TestEmailWorkerClaimsEachRowOnce(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "31CS008"})
	for i := 0; i < 20; i++ {
		newEmailOutbox(t, e, s, "Concurrent case")
	}
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go func() {
			for {
				n, err := e.App.Email.ProcessOnce(e.Ctx)
				if err != nil || n == 0 {
					break
				}
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if got := e.EmailSender.Count(); got != 20 {
		t.Fatalf("%d emails sent for 20 queued (duplicates or losses)", got)
	}
}
