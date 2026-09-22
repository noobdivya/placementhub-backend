package app_test

import (
	"strings"
	"testing"

	"placementhub/internal/domain"
	"placementhub/internal/testutil"
)

// The real branch list, not a hand-maintained copy — so this test can't drift
// out of sync with internal/domain.Branches the way it just did.
var allBranches = domain.Branches

func TestReportsMatchTheUnderlyingData(t *testing.T) {
	w := newWorld(t)
	e := w.e
	quantra := e.Company("Quantra Systems", true)
	qw := &world{e: e, admin: w.admin, company: quantra}

	// Five students across branches; four end up placed at varied CTCs.
	type placed struct {
		roll, branch string
		co           *world
		ctc          float64
	}
	plan := []placed{
		{"R1", "CSE", w, 5.0},  // < 6
		{"R2", "CSE", w, 12.0}, // 10-15
		{"R3", "IT", qw, 30.0}, // 25+
		{"R4", "ECE", qw, 6.0}, // 6-10 (boundary: 6 belongs to 6-10)
	}
	for _, p := range plan {
		s := e.Student(testutil.StudentOpts{Roll: p.roll, Branch: p.branch})
		job := e.Job(p.co.company.CompanyID, testutil.JobOpts{Role: "Role " + p.roll, Branches: allBranches})
		app := p.co.mustApply(s, job)
		p.co.mustMove(app, "Shortlisted")
		p.co.mustMove(app, "Interview")
		if r := p.co.move(app, "Offered", map[string]any{"ctc": p.ctc}); r.Status != 200 {
			t.Fatalf("offer: %d %s", r.Status, r.Body)
		}
		var offerID string
		_ = e.Pool.QueryRow(e.Ctx, `SELECT id::text FROM offers WHERE application_id = $1`, app).Scan(&offerID)
		if r := p.co.accept(s, offerID); r.Status != 200 {
			t.Fatalf("accept: %d %s", r.Status, r.Body)
		}
	}
	unplaced := e.Student(testutil.StudentOpts{Roll: "R5", Branch: "EEE"})
	// An offer that is made but not accepted counts as an offer, not a placement.
	pendingApp := w.mustApply(unplaced, e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Pending", Branches: allBranches}))
	w.offer(pendingApp)
	// A rescinded offer counts nowhere.
	rescinded := e.Student(testutil.StudentOpts{Roll: "R6", Branch: "EEE"})
	ra := w.mustApply(rescinded, e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Rescinded", Branches: allBranches}))
	w.offer(ra)
	w.mustMove(ra, "Rejected")

	sum := e.Get(w.admin, "/admin/reports/summary").JSON()
	want := map[string]float64{"totalStudents": 6, "placed": 4, "avgCtc": 13.3, "highestCtc": 30, "offers": 5, "companiesVisited": 2}
	for k, v := range want {
		if sum[k].(float64) != v {
			t.Errorf("summary.%s = %v, want %v", k, sum[k], v)
		}
	}

	bands := map[string]float64{}
	for _, b := range decodeArray(e.Get(w.admin, "/admin/reports/ctc-bands")) {
		bands[b["label"].(string)] = b["value"].(float64)
	}
	for label, n := range map[string]float64{"< 6 LPA": 1, "6–10": 1, "10–15": 1, "15–25": 0, "25+": 1} {
		if bands[label] != n {
			t.Errorf("band %q = %v, want %v (all: %v)", label, bands[label], n, bands)
		}
	}

	branches := decodeArray(e.Get(w.admin, "/admin/reports/branches"))
	if len(branches) != len(domain.Branches) || branches[0]["branch"] != domain.Branches[0] ||
		branches[len(branches)-1]["branch"] != domain.Branches[len(domain.Branches)-1] {
		t.Fatalf("branches = %v", branches)
	}
	got := map[string][2]float64{}
	for _, b := range branches {
		got[b["branch"].(string)] = [2]float64{b["total"].(float64), b["placed"].(float64)}
	}
	wantBranch := map[string][2]float64{"CSE": {2, 2}, "IT": {1, 1}, "ECE": {1, 1}, "EEE": {2, 0}}
	for _, b := range domain.Branches {
		if _, ok := wantBranch[b]; !ok {
			wantBranch[b] = [2]float64{0, 0} // every other branch has no students in this test
		}
	}
	for br, tp := range wantBranch {
		if got[br] != tp {
			t.Errorf("branch %s total/placed = %v, want %v", br, got[br], tp)
		}
	}

	top := decodeArray(e.Get(w.admin, "/admin/reports/top-recruiters"))
	if len(top) != 2 || top[0]["hires"].(float64) != 2 || top[1]["hires"].(float64) != 2 {
		t.Errorf("top recruiters = %v", top)
	}
	recent := decodeArray(e.Get(w.admin, "/admin/reports/recent-offers"))
	if len(recent) != 5 { // 4 accepted + 1 pending, rescinded excluded
		t.Errorf("recent offers = %d, want 5", len(recent))
	}
	months := decodeArray(e.Get(w.admin, "/admin/reports/monthly-offers"))
	if len(months) != 6 || months[5]["value"].(float64) != 5 {
		t.Errorf("monthly offers = %v", months)
	}

	ov := e.Get(w.admin, "/admin/reports/overview").JSON()
	for _, k := range []string{"stats", "monthlyOffers", "branchStats", "ctcBands", "topRecruiters", "recentOffers"} {
		if ov[k] == nil {
			t.Errorf("overview missing %q", k)
		}
	}
	csv := e.Get(w.admin, "/admin/reports/placements.csv")
	if lines := strings.Split(strings.TrimSpace(string(csv.Body)), "\n"); len(lines) != 5 {
		t.Errorf("placements.csv has %d lines, want header + 4:\n%s", len(lines), csv.Body)
	}
	// Companies and students never see reports.
	if r := e.Get(w.company, "/admin/reports/summary"); r.Status != 403 {
		t.Errorf("company reading reports: %d", r.Status)
	}
}

func decodeArray(r testutil.Resp) []map[string]any {
	var out []map[string]any
	r.Decode(&out)
	return out
}

func TestCompanyListDerivesOpenRolesAndHires(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS090"})
	e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Open one"})
	e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Open two"})
	e.Job(w.company.CompanyID, testutil.JobOpts{Role: "Closed", Status: "Closed"})
	w.placeStudent(s)

	var nimbus map[string]any
	for _, c := range e.Get(w.admin, "/admin/companies").Items() {
		if c["name"] == "Nimbus Labs" {
			nimbus = c
		}
	}
	// placeStudent adds a third open job.
	if nimbus == nil || nimbus["openRoles"].(float64) != 3 || nimbus["hires"].(float64) != 1 || nimbus["status"] != "Approved" {
		t.Errorf("company row = %v", nimbus)
	}
	// Rejecting a company pulls its open jobs.
	if r := e.Req(w.admin, "PATCH", "/admin/companies/"+w.company.CompanyID.String()+"/status", map[string]any{"status": "Rejected", "reason": "Failed verification"}); r.Status != 200 {
		t.Fatalf("reject company: %d %s", r.Status, r.Body)
	}
	if n := e.Count(`SELECT count(*) FROM jobs WHERE company_id = $1 AND status = 'Open'`, w.company.CompanyID); n != 0 {
		t.Errorf("%d jobs still open for a rejected company", n)
	}
	if r := e.Req(w.admin, "PATCH", "/admin/companies/"+w.company.CompanyID.String()+"/status", map[string]any{"status": "Bogus"}); r.Status != 422 {
		t.Errorf("bogus status: %d, want 422", r.Status)
	}
}

func TestNoticesAndSiteContent(t *testing.T) {
	w := newWorld(t)
	e := w.e
	cse := e.Student(testutil.StudentOpts{Roll: "N1", Branch: "CSE"})
	ece := e.Student(testutil.StudentOpts{Roll: "N2", Branch: "ECE"})

	// Public read needs no login.
	if r := e.Get(nil, "/notices"); r.Status != 200 || len(r.Items()) != 0 {
		t.Fatalf("empty notices: %d %s", r.Status, r.Body)
	}
	// Publish without a push, then one targeted at CSE only.
	if r := e.Post(w.admin, "/admin/notices", map[string]any{"title": "Orientation on Friday", "tag": "Event"}); r.Status != 201 || r.JSON()["studentsNotified"].(float64) != 0 {
		t.Fatalf("plain notice: %d %s", r.Status, r.Body)
	}
	r := e.Post(w.admin, "/admin/notices", map[string]any{"title": "CSE drive shortlist out", "tag": "Drive", "notify": true, "branches": []string{"CSE"}})
	if r.Status != 201 || r.JSON()["studentsNotified"].(float64) != 1 {
		t.Fatalf("targeted notice: %d %s", r.Status, r.Body)
	}
	got := w.notifiedUsers("notice")
	if !got[cse.ID] || got[ece.ID] {
		t.Errorf("notice recipients = %v", got)
	}
	items := e.Get(nil, "/notices").Items()
	if len(items) != 2 || items[0]["title"] != "CSE drive shortlist out" || items[0]["tag"] != "Drive" || items[0]["date"] == nil {
		t.Errorf("notices = %v", items)
	}
	if r := e.Post(w.admin, "/admin/notices", map[string]any{"title": "x", "tag": "Gossip"}); r.Status != 422 {
		t.Errorf("bad notice: %d, want 422", r.Status)
	}
	if r := e.Post(cse, "/admin/notices", map[string]any{"title": "Fake notice", "tag": "Alert"}); r.Status != 403 {
		t.Errorf("student publishing a notice: %d, want 403", r.Status)
	}
	id := items[1]["id"].(string)
	if r := e.Req(w.admin, "DELETE", "/admin/notices/"+id, nil); r.Status != 204 {
		t.Errorf("delete notice: %d", r.Status)
	}

	// Site config: public read, admin write.
	if r := e.Get(nil, "/site-config"); r.Status != 200 || string(r.Body) != "{}" {
		t.Errorf("default site config: %d %s", r.Status, r.Body)
	}
	cfg := `{"name":"Placement Hub","college":"Test College","phones":["+91 00000 00000"]}`
	if r := e.Req(w.admin, "PUT", "/admin/site-config", cfg); r.Status != 204 {
		t.Fatalf("set config: %d %s", r.Status, r.Body)
	}
	if got := e.Get(nil, "/site-config").JSON(); got["college"] != "Test College" {
		t.Errorf("site config = %v", got)
	}
	if r := e.Req(w.admin, "PUT", "/admin/site-config", `["not","an","object"]`); r.Status != 400 {
		t.Errorf("array as config: %d, want 400", r.Status)
	}
	if r := e.Req(cse, "PUT", "/admin/site-config", cfg); r.Status != 403 {
		t.Errorf("student setting config: %d, want 403", r.Status)
	}
	// Recruiters: approved companies only.
	e.Company("Pending Co", false)
	rec := e.Get(nil, "/recruiters").Items()
	if len(rec) != 1 || rec[0]["name"] != "Nimbus Labs" {
		t.Errorf("recruiters = %v", rec)
	}
}
