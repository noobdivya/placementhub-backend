package app_test

import (
	"net/http"
	"net/url"
	"testing"

	"placementhub/internal/testutil"

	"github.com/google/uuid"
)

// One job, many students: every eligibility rule and the exact error code.
func TestApplyEligibilityRules(t *testing.T) {
	w := newWorld(t)
	e := w.e
	job := e.Job(w.company.CompanyID, testutil.JobOpts{MinCGPA: 7.5, Branches: []string{"CSE", "IT"}, AllowBacklogs: false})
	backlogOK := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Backlogs welcome", MinCGPA: 7.0, AllowBacklogs: true})

	cases := []struct {
		name     string
		opts     testutil.StudentOpts
		job      uuid.UUID
		wantCode int
		wantErr  string
	}{
		{"eligible", testutil.StudentOpts{Roll: "E1", CGPA: 8.0, Branch: "CSE"}, job, 201, ""},
		{"exactly at the cutoff", testutil.StudentOpts{Roll: "E2", CGPA: 7.5, Branch: "IT"}, job, 201, ""},
		{"cgpa just below", testutil.StudentOpts{Roll: "E3", CGPA: 7.49, Branch: "CSE"}, job, 422, "cgpa_too_low"},
		{"wrong branch", testutil.StudentOpts{Roll: "E4", CGPA: 9.0, Branch: "Mechanical"}, job, 422, "branch_not_eligible"},
		{"active backlog", testutil.StudentOpts{Roll: "E5", CGPA: 9.0, Branch: "CSE", Backlogs: 1}, job, 422, "backlogs_not_allowed"},
		{"backlog allowed by the job", testutil.StudentOpts{Roll: "E6", CGPA: 7.2, Branch: "ECE", Backlogs: 2}, backlogOK, 201, ""},
		{"no resume", testutil.StudentOpts{Roll: "E7", CGPA: 9.0, Branch: "CSE", NoResume: true}, job, 422, "resume_required"},
	}
	for _, c := range cases {
		s := e.Student(c.opts)
		r := w.apply(s, c.job)
		if r.Status != c.wantCode || (c.wantErr != "" && r.ErrCode() != c.wantErr) {
			t.Errorf("%s: got %d %s, want %d %s", c.name, r.Status, r.Body, c.wantCode, c.wantErr)
		}
	}
}

func TestApplyRejectsDuplicatesClosedAndExpiredJobs(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS020"})
	open := e.Job(w.company.CompanyID, testutil.JobOpts{})

	w.mustApply(s, open)
	if r := w.apply(s, open); r.Status != 409 || r.ErrCode() != "already_applied" {
		t.Errorf("duplicate: %d %s", r.Status, r.Body)
	}
	for name, st := range map[string]string{"Closed": "Closed", "Draft": "Draft", "Pending": "Pending"} {
		id := e.Job(w.company.CompanyID, testutil.JobOpts{Role: name, Status: st})
		if r := w.apply(s, id); r.Status != 422 || r.ErrCode() != "job_closed" {
			t.Errorf("%s job: %d %s, want 422 job_closed", name, r.Status, r.Body)
		}
	}
	// Deadline is the whole deadline day, and no longer.
	expired := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Expired", DeadlineDays: -2})
	if r := w.apply(s, expired); r.Status != 422 || r.ErrCode() != "deadline_passed" {
		t.Errorf("expired deadline: %d %s", r.Status, r.Body)
	}
	if r := w.apply(s, uuid.New()); r.Status != 404 {
		t.Errorf("unknown job: %d, want 404", r.Status)
	}
	if r := e.Post(s, "/jobs/not-a-uuid/apply", nil); r.Status != 404 {
		t.Errorf("malformed job id: %d, want 404", r.Status)
	}
}

func TestStudentJobListShowsPerStudentEligibilityAndFilters(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS021", CGPA: 7.8, Branch: "CSE"})
	easy := e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Backend Engineer", CTC: 16, MinCGPA: 7.0})
	e.Job(w.company.CompanyID, testutil.JobOpts{Role: "ML Engineer", CTC: 22, MinCGPA: 8.5})
	e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Embedded", CTC: 10, Branches: []string{"ECE", "EEE"}})
	e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Old", Status: "Closed"})
	e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Expired", DeadlineDays: -1})
	e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Hidden draft", Status: "Draft"})

	all := e.Get(s, "/jobs").Items()
	if len(all) != 3 {
		t.Fatalf("listed %d jobs, want the 3 live ones", len(all))
	}
	eligible := e.Get(s, "/jobs?eligible=true").Items()
	if len(eligible) != 1 || eligible[0]["role"] != "Backend Engineer" {
		t.Fatalf("eligible list = %v", eligible)
	}
	if r := e.Get(s, "/jobs?sort=ctc").Items(); r[0]["role"] != "ML Engineer" {
		t.Errorf("sort=ctc first = %v", r[0]["role"])
	}
	if r := e.Get(s, "/jobs?q="+url.QueryEscape("embed")).Items(); len(r) != 1 {
		t.Errorf("search returned %d, want 1", len(r))
	}
	// Reasons per job.
	byRole := map[string]map[string]any{}
	for _, j := range all {
		byRole[j["role"].(string)] = j
	}
	if br := byRole["ML Engineer"]["blockReason"].(map[string]any); br["code"] != "cgpa_too_low" {
		t.Errorf("ML reason = %v", br)
	}
	if br := byRole["Embedded"]["blockReason"].(map[string]any); br["code"] != "branch_not_eligible" {
		t.Errorf("Embedded reason = %v", br)
	}
	if byRole["Backend Engineer"]["canApply"] != true {
		t.Errorf("Backend should be applyable")
	}

	// After applying, the flags flip and the job stays listed.
	w.mustApply(s, easy)
	j := e.Get(s, "/jobs/"+easy.String()).JSON()
	if j["applied"] != true || j["canApply"] != false || j["applicationId"] == nil {
		t.Errorf("after applying: %v", j)
	}
	if br := j["blockReason"].(map[string]any); br["code"] != "already_applied" {
		t.Errorf("reason after applying = %v", br)
	}
	// A closed job is still visible to someone who applied to it, but not to others.
	e.MustExec(`UPDATE jobs SET status = 'Closed' WHERE id = $1`, easy)
	if r := e.Get(s, "/jobs/"+easy.String()); r.Status != 200 {
		t.Errorf("closed job hidden from its applicant: %d", r.Status)
	}
	stranger := e.Student(testutil.StudentOpts{Roll: "21CS022"})
	if r := e.Get(stranger, "/jobs/"+easy.String()); r.Status != 404 {
		t.Errorf("closed job visible to a non-applicant: %d, want 404", r.Status)
	}
}

func jobBody(deadline string, over map[string]any) map[string]any {
	b := map[string]any{
		"role": "Backend Engineer", "type": "Full-time", "location": "Gurugram", "ctc": 16, "minCgpa": 7.5,
		"branches": []string{"IT", "CSE"}, "skills": []string{"Node.js", "PostgreSQL"}, "openings": 7,
		"description": "Design high-throughput ingestion pipelines.", "deadline": deadline, "allowBacklogs": false,
	}
	for k, v := range over {
		b[k] = v
	}
	return b
}

func TestJobLifecycleDraftSubmitApproveReject(t *testing.T) {
	w := newWorld(t)
	e := w.e
	deadline := e.Clock.Now().In(e.Cfg.Location).AddDate(0, 0, 15).Format("2006-01-02")
	s := e.Student(testutil.StudentOpts{Roll: "21CS023"})

	// Validation reports every bad field at once.
	bad := e.Post(w.company, "/company/jobs", map[string]any{"role": "x", "type": "Gig", "branches": []string{}, "deadline": "yesterday"})
	if bad.Status != 422 {
		t.Fatalf("invalid job: %d %s", bad.Status, bad.Body)
	}
	fields := bad.JSON()["error"].(map[string]any)["fields"].(map[string]any)
	for _, f := range []string{"role", "type", "branches", "deadline", "openings", "description"} {
		if _, ok := fields[f]; !ok {
			t.Errorf("no validation error for %q: %v", f, fields)
		}
	}
	if r := e.Post(w.company, "/company/jobs", jobBody(e.Clock.Now().AddDate(0, 0, -3).Format("2006-01-02"), nil)); r.Status != 422 {
		t.Errorf("past deadline accepted: %d", r.Status)
	}
	if r := e.Post(w.company, "/company/jobs", jobBody(deadline, map[string]any{"branches": []string{"Astrology"}})); r.Status != 422 {
		t.Errorf("unknown branch accepted: %d", r.Status)
	}

	created := e.Post(w.company, "/company/jobs", jobBody(deadline, nil))
	if created.Status != 201 {
		t.Fatalf("create: %d %s", created.Status, created.Body)
	}
	id := created.JSON()["id"].(string)
	if created.JSON()["status"] != "Draft" {
		t.Fatalf("new job should be a Draft")
	}
	if n := len(e.Get(s, "/jobs").Items()); n != 0 {
		t.Fatalf("a draft is visible to students")
	}

	// Edit the draft freely, then submit.
	if r := e.Req(w.company, "PUT", "/company/jobs/"+id, jobBody(deadline, map[string]any{"ctc": 18, "minCgpa": 8})); r.Status != 200 || r.JSON()["ctc"].(float64) != 18 {
		t.Fatalf("edit draft: %d %s", r.Status, r.Body)
	}
	if r := e.Post(w.company, "/company/jobs/"+id+"/submit", nil); r.Status != 200 || r.JSON()["status"] != "Pending" {
		t.Fatalf("submit: %d %s", r.Status, r.Body)
	}
	if n := len(e.Get(s, "/jobs").Items()); n != 0 {
		t.Fatalf("a pending job is visible to students")
	}
	if r := e.Req(w.company, "PUT", "/company/jobs/"+id, jobBody(deadline, nil)); r.Status != 409 {
		t.Errorf("editing a pending job: %d, want 409", r.Status)
	}
	if r := e.Post(w.company, "/company/jobs/"+id+"/submit", nil); r.Status != 409 {
		t.Errorf("double submit: %d, want 409", r.Status)
	}

	// Admin rejects with a reason; the company sees it, edits and resubmits.
	if r := e.Post(w.admin, "/admin/jobs/"+id+"/reject", map[string]any{"reason": ""}); r.Status != 422 {
		t.Errorf("reject without reason: %d, want 422", r.Status)
	}
	if r := e.Post(w.admin, "/admin/jobs/"+id+"/reject", map[string]any{"reason": "CTC looks like a typo"}); r.Status != 200 || r.JSON()["status"] != "Rejected" {
		t.Fatalf("reject: %d %s", r.Status, r.Body)
	}
	got := e.Get(w.company, "/company/jobs/"+id).JSON()
	if got["status"] != "Rejected" || got["rejectReason"] != "CTC looks like a typo" {
		t.Fatalf("company view of rejection: %v", got)
	}
	e.Req(w.company, "PUT", "/company/jobs/"+id, jobBody(deadline, map[string]any{"ctc": 16}))
	e.Post(w.company, "/company/jobs/"+id+"/submit", nil)

	// Approval makes it live.
	ap := e.Post(w.admin, "/admin/jobs/"+id+"/approve", nil)
	if ap.Status != 200 || ap.JSON()["job"].(map[string]any)["status"] != "Open" {
		t.Fatalf("approve: %d %s", ap.Status, ap.Body)
	}
	if n := len(e.Get(s, "/jobs").Items()); n != 1 {
		t.Fatalf("approved job not listed for students")
	}
	if r := e.Post(w.admin, "/admin/jobs/"+id+"/approve", nil); r.Status != 409 {
		t.Errorf("approving twice: %d, want 409", r.Status)
	}

	// Once live, the eligibility criteria are frozen; the deadline may be extended.
	if r := e.Req(w.company, "PUT", "/company/jobs/"+id, jobBody(deadline, map[string]any{"minCgpa": 6.0})); r.Status != 409 || r.ErrCode() != "criteria_locked" {
		t.Errorf("changing criteria of a live job: %d %s", r.Status, r.Body)
	}
	later := e.Clock.Now().In(e.Cfg.Location).AddDate(0, 0, 25).Format("2006-01-02")
	if r := e.Req(w.company, "PUT", "/company/jobs/"+id, jobBody(later, map[string]any{"openings": 12})); r.Status != 200 || r.JSON()["deadline"] != later {
		t.Errorf("extending a live job's deadline: %d %s", r.Status, r.Body)
	}
	if r := e.Post(w.company, "/company/jobs/"+id+"/close", nil); r.Status != 200 || r.JSON()["status"] != "Closed" {
		t.Errorf("close: %d %s", r.Status, r.Body)
	}
	if n := len(e.Get(s, "/jobs").Items()); n != 0 {
		t.Errorf("closed job still listed")
	}
	if r := e.Req(w.company, "DELETE", "/company/jobs/"+id, nil); r.Status != 409 {
		t.Errorf("deleting a non-draft job: %d, want 409", r.Status)
	}
}

func TestCompaniesCannotTouchEachOthersJobsOrCandidates(t *testing.T) {
	w := newWorld(t)
	e := w.e
	rival := e.Company("Rival Corp", true)
	deadline := e.Clock.Now().In(e.Cfg.Location).AddDate(0, 0, 15).Format("2006-01-02")
	mine := e.Post(w.company, "/company/jobs", jobBody(deadline, nil)).JSON()["id"].(string)

	for _, c := range []struct{ method, path string }{
		{"GET", "/company/jobs/" + mine},
		{"PUT", "/company/jobs/" + mine},
		{"DELETE", "/company/jobs/" + mine},
		{"POST", "/company/jobs/" + mine + "/submit"},
		{"POST", "/company/jobs/" + mine + "/close"},
	} {
		var body any
		if c.method == "PUT" {
			body = jobBody(deadline, nil)
		}
		if r := e.Req(rival, c.method, c.path, body); r.Status != 404 {
			t.Errorf("rival %s %s = %d, want 404", c.method, c.path, r.Status)
		}
	}
	if n := len(e.Get(rival, "/company/jobs").Items()); n != 0 {
		t.Errorf("rival lists %d of someone else's jobs", n)
	}

	// Candidates.
	s := e.Student(testutil.StudentOpts{Roll: "21CS024"})
	app := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{}))
	if n := len(e.Get(rival, "/company/candidates").Items()); n != 0 {
		t.Errorf("rival sees %d candidates of another company", n)
	}
	for _, path := range []string{"/company/applications/" + app, "/company/applications/" + app + "/resume"} {
		if r := e.Get(rival, path); r.Status != 404 {
			t.Errorf("rival GET %s = %d, want 404", path, r.Status)
		}
	}
	if r := (&world{e: e, admin: w.admin, company: rival}).move(app, "Shortlisted", nil); r.Status != 404 {
		t.Errorf("rival moving a candidate: %d, want 404", r.Status)
	}
	if got := w.stageOf(s, app); got != "Applied" {
		t.Errorf("candidate moved by a rival: %s", got)
	}
}

func TestStageTransitionRules(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS025"})
	app := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{}))

	// Steps cannot be skipped; unknown stages are rejected; no-ops are refused.
	for _, c := range []struct {
		stage string
		want  int
	}{{"Interview", 422}, {"Offered", 422}, {"Applied", 409}, {"Withdrawn", 422}, {"Nonsense", 422}} {
		if r := w.move(app, c.stage, nil); r.Status != c.want {
			t.Errorf("Applied -> %s = %d %s, want %d", c.stage, r.Status, r.Body, c.want)
		}
	}
	w.mustMove(app, "Shortlisted")
	// A note alone can update the interview slot without changing the stage.
	if r := w.move(app, "Shortlisted", map[string]any{"note": "Awaiting slot"}); r.Status != 200 || r.JSON()["note"] != "Awaiting slot" {
		t.Errorf("note-only update: %d %s", r.Status, r.Body)
	}
	w.mustMove(app, "Interview")
	// Rejecting and resetting are always available.
	w.mustMove(app, "Rejected")
	if got := w.stageOf(s, app); got != "Rejected" {
		t.Errorf("stage = %s", got)
	}
	w.mustMove(app, "Applied")
	if got := w.stageOf(s, app); got != "Applied" {
		t.Errorf("stage after reset = %s", got)
	}
	// Reaching Offered creates a pending offer; leaving it rescinds the offer.
	oid := w.offer(app)
	if r := w.move(app, "Rejected", nil); r.Status != 200 {
		t.Fatalf("reject offered: %d %s", r.Status, r.Body)
	}
	for _, o := range e.Get(s, "/me/offers").Items() {
		if o["id"] == oid && o["status"] != "Rescinded" {
			t.Errorf("offer status after the company rejected = %v, want Rescinded", o["status"])
		}
	}
	if r := w.accept(s, oid); r.Status != 409 {
		t.Errorf("accepting a rescinded offer: %d, want 409", r.Status)
	}
	// Every move is in the audit trail.
	if n := e.Count(`SELECT count(*) FROM application_events WHERE application_id = $1`, app); n < 8 {
		t.Errorf("only %d history events recorded", n)
	}
	// Offer bounds are validated.
	app2 := w.mustApply(e.Student(testutil.StudentOpts{Roll: "21CS026"}), e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Second"}))
	w.mustMove(app2, "Shortlisted")
	w.mustMove(app2, "Interview")
	if r := w.move(app2, "Offered", map[string]any{"validUntil": "2020-01-01T00:00:00Z"}); r.Status != 422 {
		t.Errorf("offer valid in the past: %d, want 422", r.Status)
	}
	if r := w.move(app2, "Offered", map[string]any{"ctc": 21.5}); r.Status != 200 || r.JSON()["offer"].(map[string]any)["ctc"].(float64) != 21.5 {
		t.Errorf("custom offer CTC: %d %s", r.Status, r.Body)
	}
}

func TestStudentWithdrawKeepsHistory(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS027"})
	app := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{}))

	if r := e.Post(s, "/me/applications/"+app+"/withdraw", nil); r.Status != http.StatusOK || r.JSON()["stage"] != "Withdrawn" {
		t.Fatalf("withdraw: %d %s", r.Status, r.Body)
	}
	if r := e.Post(s, "/me/applications/"+app+"/withdraw", nil); r.Status != 409 {
		t.Errorf("withdrawing twice: %d, want 409", r.Status)
	}
	if n := len(e.Get(s, "/me/applications").Items()); n != 1 {
		t.Errorf("withdrawn application vanished from history")
	}
	// The recruiter cannot revive it, and it is hidden from the board by default.
	if r := w.move(app, "Shortlisted", nil); r.Status != 409 {
		t.Errorf("moving a withdrawn application: %d, want 409", r.Status)
	}
	if n := len(e.Get(w.company, "/company/candidates").Items()); n != 0 {
		t.Errorf("withdrawn candidate on the default board")
	}
	if n := len(e.Get(w.company, "/company/candidates?includeWithdrawn=true").Items()); n != 1 {
		t.Errorf("includeWithdrawn=true returned %d, want 1", n)
	}
	// Another student cannot withdraw it.
	other := e.Student(testutil.StudentOpts{Roll: "21CS028"})
	if r := e.Post(other, "/me/applications/"+app+"/withdraw", nil); r.Status != 404 {
		t.Errorf("withdrawing someone else's application: %d, want 404", r.Status)
	}
}
