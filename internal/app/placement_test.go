package app_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"placementhub/internal/db"
	"placementhub/internal/testutil"

	"github.com/google/uuid"
)

// The headline rule: accepting an offer makes the student PLACED and blocks
// every further application or drive registration, while keeping history.
func TestAcceptOfferMarksPlacedAndBlocksFurtherApplications(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS001", Name: "Placed Student"})
	other := e.Company("Quantra Systems", true)

	j1 := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "SWE", CTC: 18})
	j2 := e.Job(other.CompanyID, testutil.JobOpts{Role: "Analyst", CTC: 12})
	j3 := e.Job(other.CompanyID, testutil.JobOpts{Role: "Frontend"})
	j4 := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "SRE"}) // never applied to before placement

	a1, a2, a3 := w.mustApply(s, j1), w.mustApply(s, j2), w.mustApply(s, j3)

	// A future drive the student registered for.
	day := e.Clock.Now().In(e.Cfg.Location).AddDate(0, 0, 3).Format("2006-01-02")
	dr := e.Post(w.admin, "/admin/drives", map[string]any{
		"companyId": w.company.CompanyID, "title": "Campus Drive", "date": day, "time": "10:00", "mode": "On-campus", "venue": "Hall A",
	})
	if dr.Status != http.StatusCreated {
		t.Fatalf("create drive: %d %s", dr.Status, dr.Body)
	}
	driveID := dr.JSON()["id"].(string)
	if r := e.Post(s, "/drives/"+driveID+"/register", nil); r.Status != http.StatusOK {
		t.Fatalf("register: %d %s", r.Status, r.Body)
	}

	// Two companies both make offers; the student is not placed until they accept.
	offer1 := w.offer(a1)
	otherCo := &world{e: e, admin: w.admin, company: other}
	offer2 := otherCo.offer(a2)
	if got := w.placementStatus(s); got != "In process" {
		t.Fatalf("before accepting, status = %q, want In process", got)
	}

	// --- accept offer 1 ---
	r := w.accept(s, offer1)
	if r.Status != http.StatusOK {
		t.Fatalf("accept: %d %s", r.Status, r.Body)
	}
	res := r.JSON()
	if res["status"] != "Placed" {
		t.Errorf("accept status = %v, want Placed", res["status"])
	}
	if res["applicationsWithdrawn"].(float64) != 2 { // a2 (offered) and a3 (applied)
		t.Errorf("applicationsWithdrawn = %v, want 2", res["applicationsWithdrawn"])
	}
	if res["offersDeclined"].(float64) != 1 {
		t.Errorf("offersDeclined = %v, want 1", res["offersDeclined"])
	}
	if res["driveRegistrationsCancelled"].(float64) != 1 {
		t.Errorf("driveRegistrationsCancelled = %v, want 1", res["driveRegistrationsCancelled"])
	}

	// Status is PLACED, with the company and CTC.
	prof := e.Get(s, "/me/profile").JSON()["placement"].(map[string]any)
	if prof["status"] != "Placed" || prof["company"] != "Nimbus Labs" || prof["ctc"].(float64) != 18 {
		t.Errorf("placement = %v", prof)
	}

	// History is intact: every application is still visible, in the right state.
	if got := w.stageOf(s, a1); got != "Offered" {
		t.Errorf("accepted application stage = %s, want Offered", got)
	}
	if got := w.stageOf(s, a2); got != "Withdrawn" {
		t.Errorf("other offered application stage = %s, want Withdrawn", got)
	}
	if got := w.stageOf(s, a3); got != "Withdrawn" {
		t.Errorf("pending application stage = %s, want Withdrawn", got)
	}
	if n := len(e.Get(s, "/me/applications").Items()); n != 3 {
		t.Errorf("history has %d applications, want 3", n)
	}
	for _, o := range e.Get(s, "/me/offers").Items() {
		switch o["id"] {
		case offer1:
			if o["status"] != "Accepted" {
				t.Errorf("offer1 status = %v", o["status"])
			}
		case offer2:
			if o["status"] != "Declined" {
				t.Errorf("offer2 status = %v, want Declined", o["status"])
			}
		}
	}

	// --- no further applications or drive registrations ---
	if r := w.apply(s, j4); r.Status != http.StatusForbidden || r.ErrCode() != "already_placed" {
		t.Errorf("apply after placement: %d %s, want 403 already_placed", r.Status, r.Body)
	}
	drive2 := e.Post(w.admin, "/admin/drives", map[string]any{
		"companyId": other.CompanyID, "title": "Second Drive", "date": day, "time": "14:00", "mode": "Virtual", "venue": "Meet",
	}).JSON()["id"].(string)
	if r := e.Post(s, "/drives/"+drive2+"/register", nil); r.Status != http.StatusForbidden || r.ErrCode() != "already_placed" {
		t.Errorf("drive register after placement: %d %s, want 403 already_placed", r.Status, r.Body)
	}

	// Jobs list says so, per job.
	for _, it := range e.Get(s, "/jobs").Items() {
		if it["canApply"] != false || it["eligible"] != false {
			t.Errorf("job %v: canApply=%v eligible=%v, want false/false", it["role"], it["canApply"], it["eligible"])
		}
		if br, _ := it["blockReason"].(map[string]any); br == nil || br["code"] == nil {
			t.Errorf("job %v has no blockReason", it["role"])
		}
	}
	if r := e.Get(s, "/jobs?eligible=true"); len(r.Items()) != 0 {
		t.Errorf("eligible=true returned %d jobs for a placed student, want 0", len(r.Items()))
	}

	// Recruiters can no longer change the accepted or withdrawn applications.
	if r := w.move(a1, "Rejected", nil); r.Status != http.StatusConflict || r.ErrCode() != "offer_accepted" {
		t.Errorf("moving accepted application: %d %s", r.Status, r.Body)
	}
	if r := otherCo.move(a3, "Shortlisted", nil); r.Status != http.StatusConflict || r.ErrCode() != "application_withdrawn" {
		t.Errorf("moving withdrawn application: %d %s", r.Status, r.Body)
	}

	// The placement cell sees the student as placed.
	list := e.Get(w.admin, "/admin/students?status=Placed").Items()
	if len(list) != 1 || list[0]["roll"] != "21CS001" || list[0]["company"] != "Nimbus Labs" {
		t.Errorf("admin placed list = %v", list)
	}
	if got := e.Get(w.admin, "/admin/reports/summary").JSON()["placed"]; got.(float64) != 1 {
		t.Errorf("report placed = %v, want 1", got)
	}

	// Accepting again is idempotent, not an error.
	if r := w.accept(s, offer1); r.Status != http.StatusOK {
		t.Errorf("re-accept: %d %s", r.Status, r.Body)
	}
	// Accepting the (auto-declined) second offer is refused.
	if r := w.accept(s, offer2); r.Status != http.StatusConflict {
		t.Errorf("accept declined offer: %d %s, want 409", r.Status, r.Body)
	}
}

// Even a code path that forgets the checks cannot create an application or
// registration for a placed student: the database itself refuses.
func TestDatabaseRefusesApplicationsForPlacedStudent(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS002"})
	j1 := e.Job(w.company.CompanyID, testutil.JobOpts{})
	j2 := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Other"})
	w.accept(s, w.offer(w.mustApply(s, j1)))

	_, err := e.Pool.Exec(e.Ctx, `INSERT INTO applications (job_id, student_id) VALUES ($1, $2)`, j2, s.StudentID)
	if db.PgCode(err) != db.CodeStudentPlaced {
		t.Fatalf("direct insert error code = %q (%v), want %s", db.PgCode(err), err, db.CodeStudentPlaced)
	}
	drive := uuid.New()
	e.Pool.Exec(e.Ctx, `INSERT INTO drives (id, company_id, title, starts_at, mode) VALUES ($1, $2, 'D', now() + interval '2 days', 'Virtual')`,
		drive, w.company.CompanyID)
	_, err = e.Pool.Exec(e.Ctx, `INSERT INTO drive_registrations (drive_id, student_id) VALUES ($1, $2)`, drive, s.StudentID)
	if db.PgCode(err) != db.CodeStudentPlaced {
		t.Fatalf("direct registration error code = %q (%v), want %s", db.PgCode(err), err, db.CodeStudentPlaced)
	}
	// And a second Accepted offer is impossible.
	_, err = e.Pool.Exec(e.Ctx, `UPDATE offers SET status = 'Accepted' WHERE student_id = $1`, s.StudentID)
	if err != nil {
		t.Fatalf("re-setting the same accepted offer should be fine: %v", err)
	}
}

// Two offers accepted at the same instant: exactly one wins.
func TestConcurrentAcceptancesPlaceExactlyOnce(t *testing.T) {
	w := newWorld(t)
	e := w.e
	other := e.Company("Quantra Systems", true)
	s := e.Student(testutil.StudentOpts{Roll: "21CS003"})
	a1 := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{}))
	a2 := w.mustApply(s, e.Job(other.CompanyID, testutil.JobOpts{}))
	o1 := w.offer(a1)
	o2 := (&world{e: e, admin: w.admin, company: other}).offer(a2)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, o := range []string{o1, o2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = w.accept(s, o).Status
		}()
	}
	wg.Wait()

	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected status %d (codes=%v)", c, codes)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("codes = %v, want exactly one 200 and one 409", codes)
	}
	if n := e.Count(`SELECT count(*) FROM offers WHERE student_id = $1 AND status = 'Accepted'`, s.StudentID); n != 1 {
		t.Fatalf("%d accepted offers, want exactly 1", n)
	}
}

// Applying while an acceptance is in flight never leaves a live application
// behind a placed student.
func TestApplyRacingAcceptanceNeverLeavesLiveApplication(t *testing.T) {
	w := newWorld(t)
	e := w.e
	for round := 0; round < 6; round++ {
		s := e.Student(testutil.StudentOpts{Roll: testutil.Sprintf("21CS1%02d", round)})
		offered := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{Role: testutil.Sprintf("Offered %d", round)}))
		offerID := w.offer(offered)
		fresh := e.Job(w.company.CompanyID, testutil.JobOpts{Role: testutil.Sprintf("Fresh %d", round)})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); w.accept(s, offerID) }()
		go func() { defer wg.Done(); w.apply(s, fresh) }()
		wg.Wait()

		if w.placementStatus(s) != "Placed" {
			t.Fatalf("round %d: student not placed", round)
		}
		live := e.Count(`SELECT count(*) FROM applications WHERE student_id = $1 AND id <> $2 AND stage IN ('Applied','Shortlisted','Interview','Offered')`,
			s.StudentID, offered)
		if live != 0 {
			t.Fatalf("round %d: placed student has %d live applications besides the accepted one", round, live)
		}
	}
}

func TestDeclineOfferWithdrawsApplicationAndKeepsStudentUnplaced(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS004"})
	a := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{}))
	o := w.offer(a)

	r := e.Post(s, "/me/offers/"+o+"/decline", map[string]any{"reason": "Better fit elsewhere"})
	if r.Status != http.StatusOK || r.JSON()["status"] != "Declined" {
		t.Fatalf("decline: %d %s", r.Status, r.Body)
	}
	if got := w.stageOf(s, a); got != "Withdrawn" {
		t.Errorf("stage = %s, want Withdrawn", got)
	}
	if got := w.placementStatus(s); got == "Placed" {
		t.Errorf("declining an offer must not place the student")
	}
	// Still free to apply elsewhere.
	if r := w.apply(s, e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Another"})); r.Status != http.StatusCreated {
		t.Errorf("apply after declining: %d %s", r.Status, r.Body)
	}
	if r := w.accept(s, o); r.Status != http.StatusConflict {
		t.Errorf("accepting a declined offer: %d, want 409", r.Status)
	}
}

func TestExpiredOfferCannotBeAccepted(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS005"})
	a := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{DeadlineDays: 30}))
	o := w.offer(a)

	e.Clock.Advance(8 * 24 * time.Hour) // default validity is 7 days
	e.Relogin(s)
	if r := w.accept(s, o); r.Status != http.StatusConflict || r.ErrCode() != "offer_expired" {
		t.Fatalf("accept after expiry: %d %s, want 409 offer_expired", r.Status, r.Body)
	}
	res, err := e.App.Scheduler.RunOnce(e.Ctx)
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	if res.OffersExpired != 1 {
		t.Errorf("OffersExpired = %d, want 1", res.OffersExpired)
	}
	if got := w.stageOf(s, a); got != "Rejected" {
		t.Errorf("stage after expiry = %s, want Rejected", got)
	}
	if w.placementStatus(s) == "Placed" {
		t.Error("expired offer must not place the student")
	}
	if n := e.Count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'offer_expired'`, s.ID); n != 1 {
		t.Errorf("offer_expired notifications = %d, want 1", n)
	}
}

func TestAdminRevokePlacementLetsStudentApplyAgain(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS006"})
	o := w.offer(w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{})))
	w.accept(s, o)
	next := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Next"})
	if r := w.apply(s, next); r.Status != http.StatusForbidden {
		t.Fatalf("precondition: apply while placed = %d", r.Status)
	}

	if r := e.Post(w.company, "/admin/offers/"+o+"/revoke", map[string]any{"reason": "x"}); r.Status != http.StatusForbidden {
		t.Errorf("a company must not revoke placements: %d", r.Status)
	}
	if r := e.Post(w.admin, "/admin/offers/"+o+"/revoke", map[string]any{"reason": ""}); r.Status != http.StatusUnprocessableEntity {
		t.Errorf("revoke without reason: %d, want 422", r.Status)
	}
	if r := e.Post(w.admin, "/admin/offers/"+o+"/revoke", map[string]any{"reason": "Company withdrew the offer"}); r.Status != http.StatusOK {
		t.Fatalf("revoke: %d %s", r.Status, r.Body)
	}
	if w.placementStatus(s) == "Placed" {
		t.Error("student still placed after revoke")
	}
	if r := w.apply(s, next); r.Status != http.StatusCreated {
		t.Errorf("apply after revoke: %d %s", r.Status, r.Body)
	}
	if r := e.Post(w.admin, "/admin/offers/"+o+"/revoke", map[string]any{"reason": "again"}); r.Status != http.StatusConflict {
		t.Errorf("double revoke: %d, want 409", r.Status)
	}
}

func TestOfferCannotBeMadeToAlreadyPlacedStudent(t *testing.T) {
	w := newWorld(t)
	e := w.e
	other := e.Company("Quantra Systems", true)
	s := e.Student(testutil.StudentOpts{Roll: "21CS007"})
	a1 := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{}))
	a2 := w.mustApply(s, e.Job(other.CompanyID, testutil.JobOpts{}))
	// Get a2 to Interview before the student is placed elsewhere.
	oc := &world{e: e, admin: w.admin, company: other}
	oc.mustMove(a2, "Shortlisted")
	oc.mustMove(a2, "Interview")

	w.accept(s, w.offer(a1)) // accepting withdraws a2
	if r := oc.move(a2, "Offered", nil); r.Status != http.StatusConflict {
		t.Fatalf("offering a placed student: %d %s, want 409", r.Status, r.Body)
	}
	if n := e.Count(`SELECT count(*) FROM offers WHERE application_id = $1`, a2); n != 0 {
		t.Errorf("an offer was created for a withdrawn application")
	}
}

func TestStudentCanOnlyActOnOwnOffers(t *testing.T) {
	w := newWorld(t)
	e := w.e
	victim := e.Student(testutil.StudentOpts{Roll: "21CS008"})
	attacker := e.Student(testutil.StudentOpts{Roll: "21CS009"})
	o := w.offer(w.mustApply(victim, e.Job(w.company.CompanyID, testutil.JobOpts{})))

	if r := w.accept(attacker, o); r.Status != http.StatusNotFound {
		t.Errorf("accepting someone else's offer: %d, want 404", r.Status)
	}
	if r := e.Post(attacker, "/me/offers/"+o+"/decline", nil); r.Status != http.StatusNotFound {
		t.Errorf("declining someone else's offer: %d, want 404", r.Status)
	}
	if got := w.placementStatus(victim); got == "Placed" {
		t.Error("victim placed by someone else")
	}
	if n := len(e.Get(attacker, "/me/offers").Items()); n != 0 {
		t.Errorf("attacker sees %d offers, want 0", n)
	}
}
