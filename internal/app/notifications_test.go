package app_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"placementhub/internal/push"
	"placementhub/internal/testutil"

	"github.com/google/uuid"
)

// placeStudent takes a student all the way to PLACED.
func (w *world) placeStudent(s *testutil.User) {
	w.e.T.Helper()
	job := w.e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Placement job " + uuid.NewString()[:4], MinCGPA: 0, Branches: []string{"CSE", "IT", "ECE", "EEE", "Mechanical", "Civil"}, AllowBacklogs: true})
	if r := w.accept(s, w.offer(w.mustApply(s, job))); r.Status != http.StatusOK {
		w.e.T.Fatalf("placeStudent: %d %s", r.Status, r.Body)
	}
}

func (w *world) postAndApproveJob(over map[string]any) string {
	w.e.T.Helper()
	deadline := w.e.Clock.Now().In(w.e.Cfg.Location).AddDate(0, 0, 15).Format("2006-01-02")
	created := w.e.Post(w.company, "/company/jobs", jobBody(deadline, over))
	if created.Status != http.StatusCreated {
		w.e.T.Fatalf("post job: %d %s", created.Status, created.Body)
	}
	id := created.JSON()["id"].(string)
	w.defineRound(id)
	r := w.e.Post(w.company, "/company/jobs/"+id+"/submit", nil)
	if r.Status != http.StatusOK || r.JSON()["status"] != "Pending" {
		w.e.T.Fatalf("submit job: %d %s", r.Status, r.Body)
	}
	return id
}

func (w *world) approve(jobID string) map[string]any {
	w.e.T.Helper()
	r := w.e.Post(w.admin, "/admin/jobs/"+jobID+"/approve", nil)
	if r.Status != http.StatusOK {
		w.e.T.Fatalf("approve: %d %s", r.Status, r.Body)
	}
	return r.JSON()
}

func (w *world) notifiedUsers(notifType string) map[uuid.UUID]bool {
	w.e.T.Helper()
	rows, err := w.e.Pool.Query(w.e.Ctx, `SELECT user_id FROM notifications WHERE type = $1`, notifType)
	if err != nil {
		w.e.T.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		_ = rows.Scan(&id)
		out[id] = true
	}
	return out
}

func TestNewJobNotifiesOnlyEligibleStudents(t *testing.T) {
	w := newWorld(t)
	e := w.e

	mk := func(roll, branch string, cgpa float64, backlogs int) *testutil.User {
		s := e.Student(testutil.StudentOpts{Roll: roll, Branch: branch, CGPA: cgpa, Backlogs: backlogs})
		e.Subscribe(s, "dev-"+roll)
		return s
	}
	eligibleA := mk("A1", "CSE", 8.0, 0)
	eligibleB := mk("B1", "IT", 8.5, 0)
	lowCGPA := mk("C1", "CSE", 6.0, 0)
	wrongBranch := mk("D1", "Mechanical", 9.0, 0)
	backlog := mk("E1", "CSE", 9.0, 1)
	placed := mk("F1", "CSE", 9.5, 0)
	w.placeStudent(placed)

	// A student who is eligible but muted new-job alerts, and one who switched push off.
	muted := mk("G1", "CSE", 8.1, 0)
	e.Req(muted, "PUT", "/me/notification-preferences", map[string]any{"categories": map[string]bool{"new_job": false}})
	pushOff := mk("H1", "CSE", 8.2, 0)
	e.Req(pushOff, "PUT", "/me/notification-preferences", map[string]any{"pushEnabled": false})
	// A student with no subscribed browser at all (eligible, gets the inbox entry only).
	noDevice := e.Student(testutil.StudentOpts{Roll: "I1", Branch: "CSE", CGPA: 8.3})

	// Drain anything queued by setup (placing a student sends them notifications).
	for {
		n, err := e.App.Push.ProcessOnce(e.Ctx)
		if err != nil || n == 0 {
			break
		}
	}
	e.Sender.Sent = nil

	id := w.postAndApproveJob(map[string]any{"role": "Platform Engineer", "minCgpa": 7.5, "branches": []string{"CSE", "IT"}})
	res := w.approve(id)

	// Exactly the five eligible, unplaced students are notified.
	if got := res["studentsNotified"].(float64); got != 5 {
		t.Errorf("studentsNotified = %v, want 5", got)
	}
	got := w.notifiedUsers("new_job")
	for name, u := range map[string]*testutil.User{"eligibleA": eligibleA, "eligibleB": eligibleB, "muted": muted, "pushOff": pushOff, "noDevice": noDevice} {
		if !got[u.ID] {
			t.Errorf("%s should have an inbox notification", name)
		}
	}
	for name, u := range map[string]*testutil.User{"lowCGPA": lowCGPA, "wrongBranch": wrongBranch, "backlog": backlog, "placed": placed} {
		if got[u.ID] {
			t.Errorf("%s must NOT be notified about a job they cannot apply to", name)
		}
	}
	if len(got) != 5 {
		t.Errorf("%d users notified, want 5", len(got))
	}

	// Push is queued only for eligible students who allow it and have a browser subscribed.
	if n := e.Count(`SELECT count(*) FROM push_outbox po JOIN notifications n ON n.id = po.notification_id WHERE n.type = 'new_job'`); n != 2 {
		t.Fatalf("%d pushes queued, want 2 (eligibleA and eligibleB)", n)
	}
	if _, err := e.App.Push.ProcessOnce(e.Ctx); err != nil {
		t.Fatal(err)
	}
	if e.Sender.Count() != 2 {
		t.Fatalf("sender got %d messages, want 2", e.Sender.Count())
	}
	endpoints := map[string]bool{}
	for _, m := range e.Sender.Sent {
		endpoints[m.Target.Endpoint] = true
		var p struct{ Title, Body, URL, Tag string }
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			t.Fatalf("payload not JSON: %s", m.Payload)
		}
		if !strings.Contains(p.Title, "Platform Engineer") || !strings.Contains(p.Title, "Nimbus Labs") {
			t.Errorf("title = %q", p.Title)
		}
		if p.URL != "/students/jobs?job="+id || p.Tag != "job_new:"+id {
			t.Errorf("url/tag = %q / %q", p.URL, p.Tag)
		}
	}
	if !endpoints["https://fcm.googleapis.com/fcm/send/dev-A1"] || !endpoints["https://fcm.googleapis.com/fcm/send/dev-B1"] {
		t.Errorf("pushed to %v", endpoints)
	}

	// Muted / switched-off students still see it in their inbox, silently.
	for name, u := range map[string]*testutil.User{"muted": muted, "pushOff": pushOff} {
		items := e.Get(u, "/me/notifications").Items()
		if len(items) != 1 || items[0]["type"] != "new_job" || items[0]["read"] != false {
			t.Errorf("%s inbox = %v", name, items)
		}
	}
	// The ineligible student's inbox is empty.
	if n := len(e.Get(lowCGPA, "/me/notifications").Items()); n != 0 {
		t.Errorf("ineligible student has %d notifications", n)
	}
}

func TestPlacedStudentStopsReceivingNewJobAlertsAndAppearsNowhereEligible(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS030"})
	w.placeStudent(s)
	w.approve(w.postAndApproveJob(map[string]any{"minCgpa": 0}))
	if w.notifiedUsers("new_job")[s.ID] {
		t.Error("placed student was notified of a new job")
	}
	for _, it := range e.Get(s, "/me/notifications").Items() {
		if it["type"] == "new_job" {
			t.Errorf("placed student's inbox contains a new-job alert: %v", it)
		}
	}
	// They did get the placement confirmation.
	if n := e.Count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'placement'`, s.ID); n != 1 {
		t.Errorf("placement confirmations = %d, want 1", n)
	}
}

func TestMuteRulesForApplicationUpdatesAndCriticalAlerts(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS031"})
	e.Subscribe(s, "dev-31")

	queued := func() int {
		return e.Count(`SELECT count(*) FROM push_outbox WHERE status = 'pending'`)
	}
	a1 := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{Role: "One"}))
	a2 := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Two"}))
	a3 := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Three"}))

	// Default: Shortlisted is pushed.
	w.mustMove(a1, "Shortlisted")
	if queued() != 1 {
		t.Fatalf("shortlist push queued = %d, want 1", queued())
	}

	// Mute application updates: Shortlisted is now inbox-only...
	if r := e.Req(s, "PUT", "/me/notification-preferences", map[string]any{"categories": map[string]bool{"application_update": false}}); r.Status != 200 {
		t.Fatalf("mute: %d %s", r.Status, r.Body)
	}
	w.mustMove(a2, "Shortlisted")
	if queued() != 1 {
		t.Errorf("muted category still queued a push (%d)", queued())
	}
	if n := e.Count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'application_update'`, s.ID); n != 2 {
		t.Errorf("inbox application_update rows = %d, want 2 (muting never hides the inbox entry)", n)
	}
	// ...but an interview invitation is critical and ignores the category mute.
	w.mustMove(a1, "Interview")
	if queued() != 2 {
		t.Errorf("interview push not queued despite category mute (%d)", queued())
	}
	// Only the master switch silences critical alerts.
	e.Req(s, "PUT", "/me/notification-preferences", map[string]any{"pushEnabled": false})
	w.mustMove(a2, "Interview")
	w.mustMove(a3, "Shortlisted")
	if queued() != 2 {
		t.Errorf("push queued while the master switch is off (%d)", queued())
	}
	// Turning it back on resumes pushes for new events.
	e.Req(s, "PUT", "/me/notification-preferences", map[string]any{"pushEnabled": true, "categories": map[string]bool{"application_update": true}})
	w.mustMove(a3, "Rejected")
	if queued() != 3 {
		t.Errorf("push not resumed after re-enabling (%d)", queued())
	}
}

func TestPreferencesAPI(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS032"})

	p := e.Get(s, "/me/notification-preferences").JSON()
	if p["pushEnabled"] != true || p["pushAvailable"] != true || p["devices"].(float64) != 0 {
		t.Fatalf("defaults = %v", p)
	}
	cats := p["categories"].([]any)
	if len(cats) != 6 {
		t.Fatalf("%d categories, want 5 mutable + critical", len(cats))
	}
	last := cats[len(cats)-1].(map[string]any)
	if last["category"] != "critical" || last["mutable"] != false {
		t.Errorf("critical category = %v", last)
	}
	if r := e.Req(s, "PUT", "/me/notification-preferences", map[string]any{"categories": map[string]bool{"critical": false}}); r.Status != 422 {
		t.Errorf("muting critical: %d, want 422", r.Status)
	}
	if r := e.Req(s, "PUT", "/me/notification-preferences", map[string]any{"categories": map[string]bool{"weekly_digest": false}}); r.Status != 422 {
		t.Errorf("unknown category: %d, want 422", r.Status)
	}
	got := e.Req(s, "PUT", "/me/notification-preferences", map[string]any{"categories": map[string]bool{"drive": false}}).JSON()
	for _, c := range got["categories"].([]any) {
		m := c.(map[string]any)
		if (m["category"] == "drive") == m["push"].(bool) {
			t.Errorf("category %v push=%v after muting drive only", m["category"], m["push"])
		}
	}
}

func TestPushSubscriptionValidationAndOwnership(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS033"})
	keys := map[string]string{
		"p256dh": "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM",
		"auth":   "tBHItJI5svbpez7KI4CCXg",
	}
	sub := func(u *testutil.User, endpoint string, k map[string]string) testutil.Resp {
		return e.Post(u, "/me/push-subscriptions", map[string]any{"endpoint": endpoint, "keys": k})
	}

	// The server POSTs to these URLs, so anything that is not a real browser push service is refused (SSRF).
	for _, bad := range []string{
		"http://fcm.googleapis.com/fcm/send/x", "https://evil.example.com/x", "https://fcm.googleapis.com.evil.com/x",
		"https://127.0.0.1/x", "https://localhost/x", "https://169.254.169.254/latest/meta-data", "https://user:pw@fcm.googleapis.com/x",
		"https://fcm.googleapis.com:8443/x", "javascript:alert(1)", "", "https://notfcm.googleapis.com.attacker.io/x",
	} {
		if r := sub(s, bad, keys); r.Status != 422 {
			t.Errorf("endpoint %q accepted: %d %s", bad, r.Status, r.Body)
		}
	}
	for _, good := range []string{
		"https://fcm.googleapis.com/fcm/send/abc", "https://updates.push.services.mozilla.com/wpush/v2/abc",
		"https://web.push.apple.com/abc", "https://wns2-par02p.notify.windows.com/w/?token=abc",
	} {
		if r := sub(s, good, keys); r.Status != 201 {
			t.Errorf("endpoint %q rejected: %d %s", good, r.Status, r.Body)
		}
	}
	if r := sub(s, "https://fcm.googleapis.com/fcm/send/keys", map[string]string{"p256dh": "!!", "auth": "x"}); r.Status != 422 {
		t.Errorf("bad keys accepted: %d", r.Status)
	}
	if r := e.Post(w.company, "/me/push-subscriptions", map[string]any{"endpoint": "https://fcm.googleapis.com/fcm/send/z", "keys": keys}); r.Status != 403 {
		t.Errorf("company subscribing to push: %d, want 403 (push is student-only in V1)", r.Status)
	}

	// The same browser signing in as someone else moves the subscription, never duplicates it.
	other := e.Student(testutil.StudentOpts{Roll: "21CS034"})
	shared := "https://fcm.googleapis.com/fcm/send/shared-browser"
	sub(s, shared, keys)
	sub(other, shared, keys)
	if n := e.Count(`SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`, shared); n != 1 {
		t.Errorf("%d rows for one browser", n)
	}
	if n := e.Count(`SELECT count(*) FROM push_subscriptions WHERE endpoint = $1 AND user_id = $2`, shared, other.ID); n != 1 {
		t.Errorf("subscription did not move to the new owner")
	}
	// A user cannot delete someone else's subscription.
	if r := e.Req(s, "DELETE", "/me/push-subscriptions", map[string]any{"endpoint": shared}); r.Status != 204 {
		t.Errorf("delete: %d", r.Status)
	}
	if n := e.Count(`SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`, shared); n != 1 {
		t.Errorf("a user deleted another user's device")
	}
	if r := e.Req(other, "DELETE", "/me/push-subscriptions", map[string]any{"endpoint": shared}); r.Status != 204 {
		t.Errorf("delete own: %d", r.Status)
	}
	if n := e.Count(`SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`, shared); n != 0 {
		t.Errorf("own subscription not deleted")
	}
	// At most 10 devices per user: older ones are dropped.
	many := e.Student(testutil.StudentOpts{Roll: "21CS035"})
	for i := 0; i < 13; i++ {
		e.Subscribe(many, testutil.Sprintf("many-%d", i))
	}
	if n := e.Count(`SELECT count(*) FROM push_subscriptions WHERE user_id = $1`, many.ID); n != 10 {
		t.Errorf("%d devices kept, want 10", n)
	}
	// The public key endpoint needs no login.
	if r := e.Get(nil, "/push/vapid-public-key"); r.Status != 200 || r.JSON()["publicKey"] != "test-public" {
		t.Errorf("vapid key: %d %s", r.Status, r.Body)
	}
}

// newOutbox queues a push for a fresh notification to the student's device.
func newOutbox(t *testing.T, e *testutil.Env, u *testutil.User, endpoint string, read bool) (outboxID int64, subID string) {
	t.Helper()
	if err := e.Pool.QueryRow(e.Ctx, `SELECT id::text FROM push_subscriptions WHERE endpoint = $1`, endpoint).Scan(&subID); err != nil {
		t.Fatal(err)
	}
	var nid uuid.UUID
	if err := e.Pool.QueryRow(e.Ctx,
		`INSERT INTO notifications (user_id, type, category, title, body, link, dedupe_key, read_at)
		 VALUES ($1, 'new_job', 'new_job', 'T', 'B', '/x', $2, CASE WHEN $3 THEN now() END) RETURNING id`,
		u.ID, uuid.NewString(), read).Scan(&nid); err != nil {
		t.Fatal(err)
	}
	if err := e.Pool.QueryRow(e.Ctx, `INSERT INTO push_outbox (notification_id, subscription_id) VALUES ($1, $2::uuid) RETURNING id`, nid, subID).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	return outboxID, subID
}

func outboxRow(t *testing.T, e *testutil.Env, id int64) (status string, attempts int, next time.Time, lastErr string) {
	t.Helper()
	if err := e.Pool.QueryRow(e.Ctx, `SELECT status, attempts, next_attempt_at, last_error FROM push_outbox WHERE id = $1`, id).
		Scan(&status, &attempts, &next, &lastErr); err != nil {
		t.Fatal(err)
	}
	return
}

func TestPushWorkerDeliveryOutcomes(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS036"})
	ep := e.Subscribe(s, "worker-dev")
	process := func() int {
		n, err := e.App.Push.ProcessOnce(e.Ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("success", func(t *testing.T) {
		id, _ := newOutbox(t, e, s, ep, false)
		e.Sender.Status = nil
		process()
		if st, _, _, _ := outboxRow(t, e, id); st != "sent" {
			t.Errorf("status = %s, want sent", st)
		}
		if n := e.Count(`SELECT count(*) FROM push_subscriptions WHERE endpoint = $1 AND last_success_at IS NOT NULL`, ep); n != 1 {
			t.Errorf("last_success_at not recorded")
		}
	})

	t.Run("already read is not pushed", func(t *testing.T) {
		before := e.Sender.Count()
		id, _ := newOutbox(t, e, s, ep, true)
		process()
		if e.Sender.Count() != before {
			t.Errorf("a notification the student already read was pushed")
		}
		if st, _, _, _ := outboxRow(t, e, id); st != "sent" {
			t.Errorf("status = %s", st)
		}
	})

	t.Run("server error is retried with backoff then delivered", func(t *testing.T) {
		id, _ := newOutbox(t, e, s, ep, false)
		e.Sender.Status = func(push.Target) (int, error) { return 503, nil }
		process()
		st, attempts, next, lastErr := outboxRow(t, e, id)
		if st != "pending" || attempts != 1 || !strings.Contains(lastErr, "503") {
			t.Fatalf("after failure: %s attempts=%d err=%q", st, attempts, lastErr)
		}
		if !next.After(e.Clock.Now().Add(20 * time.Second)) {
			t.Errorf("no backoff: next attempt at %v (now %v)", next, e.Clock.Now())
		}
		if n := process(); n != 0 {
			t.Errorf("retried before the backoff elapsed (%d)", n)
		}
		e.Clock.Advance(push.Backoff(1) + time.Second)
		e.Sender.Status = nil
		process()
		if st, attempts, _, _ := outboxRow(t, e, id); st != "sent" || attempts != 2 {
			t.Errorf("after retry: %s attempts=%d", st, attempts)
		}
	})

	t.Run("network error is retried", func(t *testing.T) {
		id, _ := newOutbox(t, e, s, ep, false)
		e.Sender.Status = func(push.Target) (int, error) { return 0, errors.New("connection reset") }
		process()
		if st, _, _, msg := outboxRow(t, e, id); st != "pending" || !strings.Contains(msg, "connection reset") {
			t.Errorf("status=%s err=%q", st, msg)
		}
		e.Clock.Advance(time.Hour)
		e.Sender.Status = nil
		process()
	})

	t.Run("gives up after repeated failures", func(t *testing.T) {
		id, _ := newOutbox(t, e, s, ep, false)
		e.Sender.Status = func(push.Target) (int, error) { return 500, nil }
		for i := 0; i < 8; i++ {
			process()
			e.Clock.Advance(2 * time.Hour)
		}
		if st, attempts, _, _ := outboxRow(t, e, id); st != "failed" || attempts != 6 {
			t.Errorf("status=%s attempts=%d, want failed after 6", st, attempts)
		}
		e.Sender.Status = nil
	})

	t.Run("permanent rejection is not retried", func(t *testing.T) {
		id, _ := newOutbox(t, e, s, ep, false)
		e.Sender.Status = func(push.Target) (int, error) { return 403, nil }
		process()
		if st, attempts, _, _ := outboxRow(t, e, id); st != "failed" || attempts != 1 {
			t.Errorf("status=%s attempts=%d, want failed after 1", st, attempts)
		}
		e.Sender.Status = nil
	})

	t.Run("410 Gone deletes the dead subscription", func(t *testing.T) {
		id, _ := newOutbox(t, e, s, ep, false)
		e.Sender.Status = func(push.Target) (int, error) { return 410, nil }
		process()
		if n := e.Count(`SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`, ep); n != 0 {
			t.Errorf("dead subscription kept")
		}
		if n := e.Count(`SELECT count(*) FROM push_outbox WHERE id = $1`, id); n != 0 {
			t.Errorf("outbox row for a dead subscription kept")
		}
		e.Sender.Status = nil
	})
}

func TestPushWorkerClaimsEachRowOnce(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS037"})
	ep := e.Subscribe(s, "claim-dev")
	for i := 0; i < 20; i++ {
		newOutbox(t, e, s, ep, false)
	}
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ { // four workers racing over the same queue
		go func() {
			for {
				n, err := e.App.Push.ProcessOnce(e.Ctx)
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
	if got := e.Sender.Count(); got != 20 {
		t.Fatalf("%d messages sent for 20 queued (duplicates or losses)", got)
	}
}

func TestNotificationInbox(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS038"})
	other := e.Student(testutil.StudentOpts{Roll: "21CS039"})
	base := e.Clock.Now().Add(-time.Hour)
	var ids []string
	for i := 0; i < 5; i++ {
		var id string
		if err := e.Pool.QueryRow(e.Ctx,
			`INSERT INTO notifications (user_id, type, category, title, dedupe_key, created_at)
			 VALUES ($1, 'notice', 'notice', $2, $3, $4) RETURNING id::text`,
			s.ID, testutil.Sprintf("N%d", i), uuid.NewString(), base.Add(time.Duration(i)*time.Minute)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	e.MustExec(`INSERT INTO notifications (user_id, type, category, title, dedupe_key) VALUES ($1, 'notice', 'notice', 'private', 'x')`, other.ID)

	if c := e.Get(s, "/me/notifications/unread-count").JSON()["count"].(float64); c != 5 {
		t.Errorf("unread = %v, want 5", c)
	}
	// Newest first, paged with a cursor, no gaps or repeats.
	page1 := e.Get(s, "/me/notifications?limit=2").JSON()
	items1 := page1["items"].([]any)
	if len(items1) != 2 || items1[0].(map[string]any)["title"] != "N4" || page1["nextCursor"] == nil {
		t.Fatalf("page 1 = %v", page1)
	}
	page2 := e.Get(s, "/me/notifications?limit=2&cursor="+page1["nextCursor"].(string)).JSON()
	items2 := page2["items"].([]any)
	if items2[0].(map[string]any)["title"] != "N2" || items2[1].(map[string]any)["title"] != "N1" {
		t.Fatalf("page 2 = %v", page2)
	}
	page3 := e.Get(s, "/me/notifications?limit=2&cursor="+page2["nextCursor"].(string)).JSON()
	if len(page3["items"].([]any)) != 1 || page3["nextCursor"] != nil {
		t.Fatalf("page 3 = %v", page3)
	}
	if r := e.Get(s, "/me/notifications?cursor=garbage"); r.Status != 400 {
		t.Errorf("bad cursor: %d, want 400", r.Status)
	}

	// Mark read: own only.
	if r := e.Post(s, "/me/notifications/"+ids[0]+"/read", nil); r.Status != 204 {
		t.Errorf("mark read: %d", r.Status)
	}
	var otherID string
	_ = e.Pool.QueryRow(e.Ctx, `SELECT id::text FROM notifications WHERE user_id = $1`, other.ID).Scan(&otherID)
	if r := e.Post(s, "/me/notifications/"+otherID+"/read", nil); r.Status != 404 {
		t.Errorf("marking someone else's notification: %d, want 404", r.Status)
	}
	if c := e.Get(s, "/me/notifications/unread-count").JSON()["count"].(float64); c != 4 {
		t.Errorf("unread after one read = %v", c)
	}
	if n := len(e.Get(s, "/me/notifications?unread=true").Items()); n != 4 {
		t.Errorf("unread filter returned %d", n)
	}
	if r := e.Post(s, "/me/notifications/read-all", nil); r.Status != 200 || r.JSON()["updated"].(float64) != 4 {
		t.Errorf("read-all: %d %s", r.Status, r.Body)
	}
	if c := e.Get(other, "/me/notifications/unread-count").JSON()["count"].(float64); c != 1 {
		t.Errorf("read-all touched another user's notifications")
	}
}

func TestNotificationDedupe(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS040"})
	id := w.postAndApproveJob(map[string]any{"minCgpa": 0})
	w.approve(id)
	// Sending the same event again (e.g. a retried job) must not duplicate.
	e.MustExec(`UPDATE jobs SET status = 'Pending' WHERE id = $1`, id)
	w.approve(id)
	if n := e.Count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'new_job'`, s.ID); n != 1 {
		t.Errorf("%d new_job notifications after re-approval, want 1", n)
	}
}
