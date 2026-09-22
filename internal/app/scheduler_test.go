package app_test

import (
	"testing"
	"time"

	"placementhub/internal/job"
	"placementhub/internal/testutil"

	"github.com/google/uuid"
)

func (w *world) runScheduler() {
	w.e.T.Helper()
	res, err := w.e.App.Scheduler.RunOnce(w.e.Ctx)
	if err != nil {
		w.e.T.Fatalf("scheduler: %v", err)
	}
	if !res.Ran {
		w.e.T.Fatal("scheduler did not run")
	}
}

func (w *world) deadlineEnd(jobID uuid.UUID) time.Time {
	w.e.T.Helper()
	var d string
	if err := w.e.Pool.QueryRow(w.e.Ctx, `SELECT to_char(deadline, 'YYYY-MM-DD') FROM jobs WHERE id = $1`, jobID).Scan(&d); err != nil {
		w.e.T.Fatal(err)
	}
	end, err := job.DeadlineEnd(d, w.e.Cfg.Location)
	if err != nil {
		w.e.T.Fatal(err)
	}
	return end
}

// advanceTo moves the test clock so that "now" is `before` ahead of t.
func (w *world) advanceTo(t time.Time, before time.Duration) {
	w.e.Clock.Advance(t.Add(-before).Sub(w.e.Clock.Now()))
}

func TestDeadlineRemindersGoToEligibleStudentsWhoHaveNotApplied(t *testing.T) {
	w := newWorld(t)
	e := w.e
	notApplied := e.Student(testutil.StudentOpts{Roll: "21CS050", CGPA: 8})
	applied := e.Student(testutil.StudentOpts{Roll: "21CS051", CGPA: 8})
	ineligible := e.Student(testutil.StudentOpts{Roll: "21CS052", CGPA: 5})
	placed := e.Student(testutil.StudentOpts{Roll: "21CS053", CGPA: 9})
	w.placeStudent(placed)

	j := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Closing Soon", MinCGPA: 7, DeadlineDays: 4})
	w.mustApply(applied, j)
	end := w.deadlineEnd(j)
	countFor := func(u *testutil.User, kind string) int {
		return e.Count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND dedupe_key = $2`, u.ID, kind+":"+j.String())
	}

	// Well before the window: nothing.
	w.advanceTo(end, 60*time.Hour)
	w.runScheduler()
	if countFor(notApplied, "deadline_48h") != 0 {
		t.Fatal("reminder sent 60h before the deadline")
	}

	// Inside 48h: only the eligible student who has not applied.
	w.advanceTo(end, 47*time.Hour)
	w.runScheduler()
	if countFor(notApplied, "deadline_48h") != 1 {
		t.Errorf("eligible non-applicant got no 48h reminder")
	}
	for name, u := range map[string]*testutil.User{"applied": applied, "ineligible": ineligible, "placed": placed} {
		if countFor(u, "deadline_48h") != 0 {
			t.Errorf("%s student received a deadline reminder", name)
		}
	}
	if countFor(notApplied, "deadline_24h") != 0 {
		t.Errorf("24h reminder sent 47h out")
	}

	// Ticks are idempotent.
	w.runScheduler()
	w.runScheduler()
	if n := e.Count(`SELECT count(*) FROM notifications WHERE type = 'deadline'`); n != 1 {
		t.Errorf("%d deadline notifications after repeated ticks, want 1", n)
	}

	// Inside 24h: a second, distinct reminder.
	w.advanceTo(end, 23*time.Hour)
	w.runScheduler()
	if countFor(notApplied, "deadline_24h") != 1 {
		t.Errorf("no 24h reminder")
	}
	if n := e.Count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'deadline'`, notApplied.ID); n != 2 {
		t.Errorf("%d deadline reminders for the student, want 2 (48h + 24h)", n)
	}

	// Once the student applies they are not nagged again on later ticks.
	// (A brand new job whose windows have not opened yet sends nothing.)
	late := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Far away", MinCGPA: 7, DeadlineDays: 30})
	w.runScheduler()
	if n := e.Count(`SELECT count(*) FROM notifications WHERE dedupe_key LIKE '%' || $1`, late.String()); n != 0 {
		t.Errorf("reminder for a job that is a month from its deadline")
	}
}

func TestNoClosingSoonRightAfterNewJobAlert(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS054"})
	// A job approved just now with only a day to go: the 48h window opened before it went live.
	j := e.Job(w.company.CompanyID, testutil.JobOpts{DeadlineDays: 1})
	e.MustExec(`UPDATE jobs SET approved_at = now() WHERE id = $1`, j)
	w.advanceTo(w.deadlineEnd(j), 30*time.Hour)
	w.runScheduler()
	if n := e.Count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND dedupe_key = $2`, s.ID, "deadline_48h:"+j.String()); n != 0 {
		t.Error("a 48h reminder was sent for a job that only went live 30h before its deadline")
	}
}

func TestExpiredJobsAreClosedByTheScheduler(t *testing.T) {
	w := newWorld(t)
	e := w.e
	soon := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Ends soon", DeadlineDays: 1})
	later := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Ends later", DeadlineDays: 20})
	draft := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Old draft", DeadlineDays: 1, Status: "Draft"})

	w.runScheduler()
	status := func(id uuid.UUID) (s string) {
		_ = e.Pool.QueryRow(e.Ctx, `SELECT status FROM jobs WHERE id = $1`, id).Scan(&s)
		return
	}
	if status(soon) != "Open" {
		t.Fatalf("job closed before its deadline day ended")
	}
	e.Clock.Advance(3 * 24 * time.Hour)
	w.runScheduler()
	if status(soon) != "Closed" {
		t.Errorf("expired job status = %s, want Closed", status(soon))
	}
	if status(later) != "Open" || status(draft) != "Draft" {
		t.Errorf("scheduler closed the wrong jobs: later=%s draft=%s", status(later), status(draft))
	}
}

func TestOfferReminderIsCriticalAndSentOnce(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS055"})
	e.Subscribe(s, "offer-dev")
	// A student who muted every category and still gets offer deadlines: only the master switch silences those.
	e.Req(s, "PUT", "/me/notification-preferences", map[string]any{"categories": map[string]bool{
		"new_job": false, "application_update": false, "deadline": false, "drive": false, "notice": false}})
	o := w.offer(w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{DeadlineDays: 30})))

	e.Clock.Advance(3 * 24 * time.Hour)
	w.runScheduler()
	if n := e.Count(`SELECT count(*) FROM notifications WHERE type = 'offer_expiring'`); n != 0 {
		t.Fatalf("reminder sent with 4 days left")
	}
	e.Clock.Advance(3*24*time.Hour + 6*time.Hour) // ~18h left of the 7-day validity
	w.runScheduler()
	w.runScheduler()
	if n := e.Count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND dedupe_key = $2`, s.ID, "offer_expiring:"+o); n != 1 {
		t.Errorf("%d expiry reminders, want exactly 1", n)
	}
	if n := e.Count(`SELECT count(*) FROM push_outbox po JOIN notifications n ON n.id = po.notification_id WHERE n.type = 'offer_expiring'`); n != 1 {
		t.Errorf("expiry reminder push not queued despite all categories muted (%d)", n)
	}
}

func TestSchedulerRunsOnOneInstanceAtATime(t *testing.T) {
	w := newWorld(t)
	e := w.e
	// Another instance holds the advisory lock.
	tx, err := e.Pool.Begin(e.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.Ctx)
	if _, err := tx.Exec(e.Ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x504c4143454d4e54)); err != nil {
		t.Fatal(err)
	}
	res, err := e.App.Scheduler.RunOnce(e.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ran {
		t.Fatal("scheduler ran while another instance held the lock")
	}
	tx.Rollback(e.Ctx)
	if res, _ := e.App.Scheduler.RunOnce(e.Ctx); !res.Ran {
		t.Fatal("scheduler did not run once the lock was free")
	}
}

func TestSchedulerCleansUpStaleRows(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS056"})
	e.MustExec(`INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at) VALUES ($1, gen_random_uuid(), 'old', now() - interval '3 days')`, s.ID)
	e.MustExec(`INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at) VALUES ($1, gen_random_uuid(), 'fresh', now() + interval '3 days')`, s.ID)
	e.MustExec(`INSERT INTO notifications (user_id, type, category, title, dedupe_key, read_at, created_at)
	            VALUES ($1, 'notice', 'notice', 'old read', 'k1', now() - interval '100 days', now() - interval '100 days')`, s.ID)
	e.MustExec(`INSERT INTO notifications (user_id, type, category, title, dedupe_key, created_at)
	            VALUES ($1, 'notice', 'notice', 'old unread', 'k2', now() - interval '100 days')`, s.ID)
	w.runScheduler()
	if n := e.Count(`SELECT count(*) FROM refresh_tokens WHERE token_hash IN ('old', 'fresh')`); n != 1 {
		t.Errorf("refresh tokens left = %d, want only the unexpired one", n)
	}
	if n := e.Count(`SELECT count(*) FROM notifications WHERE dedupe_key IN ('k1', 'k2')`); n != 1 {
		t.Errorf("old notifications left = %d, want only the unread one", n)
	}
}
