package app_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"placementhub/internal/testutil"
)

func TestProfileEditingIsLimitedToSafeFields(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS060", CGPA: 7.2, Branch: "CSE", Name: "Real Name"})

	// Students cannot raise their own CGPA, change branch or backlogs, name, roll or email.
	r := e.Req(s, "PUT", "/me/profile", map[string]any{
		"cgpa": 9.9, "backlogs": 0, "branch": "IT", "roll": "HACKED", "email": "x@y.z", "name": "Hacker",
		"phone": "+91 98765 43210", "about": "Builder of things", "tenth": 94.2, "twelfth": 91.8,
		"links": map[string]string{"github": "github.com/me", "linkedin": "linkedin.com/in/me"},
	})
	if r.Status != 200 {
		t.Fatalf("update: %d %s", r.Status, r.Body)
	}
	p := r.JSON()
	if p["cgpa"].(float64) != 7.2 || p["branch"] != "CSE" || p["roll"] != "21CS060" || p["name"] != "Real Name" || p["email"] != s.Email {
		t.Errorf("read-only fields changed: %v", p)
	}
	if p["phone"] != "+91 98765 43210" || p["about"] != "Builder of things" || p["tenth"].(float64) != 94.2 {
		t.Errorf("editable fields not saved: %v", p)
	}
	if l := p["links"].(map[string]any); l["github"] != "github.com/me" || l["linkedin"] != "linkedin.com/in/me" {
		t.Errorf("links = %v", l)
	}
	if _, leaked := p["password_hash"]; leaked || strings.Contains(string(r.Body), "hash") {
		t.Errorf("response leaks credential fields: %s", r.Body)
	}
	// Validation.
	for name, body := range map[string]map[string]any{
		"letters in phone": {"phone": "call me maybe"},
		"percentage > 100": {"tenth": 101},
		"negative":         {"twelfth": -1},
		"huge about":       {"about": strings.Repeat("x", 2001)},
	} {
		if r := e.Req(s, "PUT", "/me/profile", body); r.Status != 422 {
			t.Errorf("%s: %d, want 422", name, r.Status)
		}
	}
	// A partial update leaves other fields alone.
	e.Req(s, "PUT", "/me/profile", map[string]any{"about": "New about"})
	if got := e.Get(s, "/me/profile").JSON(); got["phone"] != "+91 98765 43210" || got["about"] != "New about" {
		t.Errorf("partial update clobbered fields: %v", got)
	}
}

func TestSkills(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS061"})
	add := func(name string) testutil.Resp { return e.Post(s, "/me/skills", map[string]any{"skill": name}) }

	if r := add("  React  "); r.Status != 200 {
		t.Fatalf("add: %d %s", r.Status, r.Body)
	}
	add("react") // duplicate, case-insensitive
	add("C/C++")
	skills := func() []any { return e.Get(s, "/me/profile").JSON()["skills"].([]any) }
	if got := skills(); len(got) != 2 || got[0] != "React" {
		t.Fatalf("skills = %v", got)
	}
	if r := add(""); r.Status != 422 {
		t.Errorf("empty skill: %d", r.Status)
	}
	if r := add(strings.Repeat("s", 41)); r.Status != 422 {
		t.Errorf("41-char skill: %d", r.Status)
	}
	if r := e.Req(s, "DELETE", "/me/skills?name=c%2Fc%2B%2B", nil); r.Status != 200 { // skills like C/C++ need a query param
		t.Fatalf("remove: %d %s", r.Status, r.Body)
	}
	if got := skills(); len(got) != 1 {
		t.Errorf("after removing C/C++: %v", got)
	}
	for i := 0; i < 40; i++ {
		add(testutil.Sprintf("skill-%d", i))
	}
	if got := skills(); len(got) != 30 {
		t.Errorf("%d skills, want the 30 cap", len(got))
	}
	if r := add("one-too-many"); r.Status != 422 || r.ErrCode() != "too_many_skills" {
		t.Errorf("31st skill: %d %s", r.Status, r.Body)
	}
	if r := add("SKILL-3"); r.Status != 200 { // an existing skill is still accepted at the cap
		t.Errorf("re-adding an existing skill at the cap: %d", r.Status)
	}
}

func TestResumeUploadValidationDownloadAndReplacement(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS062", NoResume: true})

	// Rejected: not a PDF (even with a .pdf name), empty, executable, oversized.
	for name, data := range map[string][]byte{
		"text renamed to pdf": []byte(strings.Repeat("just some text, definitely not a pdf. ", 10)),
		"html":                []byte("<html><script>alert(1)</script></html>" + strings.Repeat(" ", 200)),
		"exe":                 append([]byte("MZ"), make([]byte, 300)...),
		"tiny":                []byte("%PDF-"),
		"empty":               {},
	} {
		if r := e.Upload(s, "/me/resume", "file", "resume.pdf", data); r.Status != 422 {
			t.Errorf("%s: %d %s, want 422", name, r.Status, r.Body)
		}
	}
	big := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("x"), 6<<20)...)
	if r := e.Upload(s, "/me/resume", "file", "big.pdf", big); r.Status != 413 {
		t.Errorf("6 MB upload: %d, want 413", r.Status)
	}
	if r := e.Upload(s, "/me/resume", "wrong-field", "resume.pdf", testutil.PDF()); r.Status != 400 {
		t.Errorf("wrong form field: %d, want 400", r.Status)
	}
	if e.Get(s, "/me/profile").JSON()["resume"] != nil {
		t.Fatal("a rejected upload left a resume behind")
	}
	if r := e.Get(s, "/me/resume"); r.Status != 404 {
		t.Errorf("download with no resume: %d, want 404", r.Status)
	}

	// Accepted, with a hostile filename reduced to a safe display name.
	pdf := testutil.PDF()
	r := e.Upload(s, "/me/resume", "file", `..\..\etc/passwd"; rm -rf.exe`, pdf)
	if r.Status != 201 {
		t.Fatalf("upload: %d %s", r.Status, r.Body)
	}
	meta := r.JSON()
	name := meta["filename"].(string)
	if strings.ContainsAny(name, `/\";`) || !strings.HasSuffix(name, ".pdf") || meta["sizeBytes"].(float64) != float64(len(pdf)) {
		t.Errorf("stored metadata = %v", meta)
	}
	dl := e.Get(s, "/me/resume")
	if dl.Status != 200 || !bytes.Equal(dl.Body, pdf) {
		t.Fatalf("download: %d, %d bytes", dl.Status, len(dl.Body))
	}
	if dl.Header.Get("Content-Type") != "application/pdf" || !strings.HasPrefix(dl.Header.Get("Content-Disposition"), "attachment") ||
		dl.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("unsafe download headers: %v", dl.Header)
	}

	// Replacing removes the old file from storage.
	var oldKey string
	_ = e.Pool.QueryRow(e.Ctx, `SELECT storage_key FROM resumes WHERE student_id = $1`, s.StudentID).Scan(&oldKey)
	pdf2 := append(append([]byte{}, pdf...), []byte("second version\n")...)
	if r := e.Upload(s, "/me/resume", "file", "v2.pdf", pdf2); r.Status != 201 {
		t.Fatalf("replace: %d %s", r.Status, r.Body)
	}
	if _, err := e.Store.Open(e.Ctx, oldKey); err == nil {
		t.Error("the replaced resume file was not deleted")
	}
	if got := e.Get(s, "/me/resume"); !bytes.Equal(got.Body, pdf2) {
		t.Error("download does not return the newest resume")
	}
	if n := e.Count(`SELECT count(*) FROM resumes WHERE student_id = $1`, s.StudentID); n != 1 {
		t.Errorf("%d resume rows, want 1", n)
	}
}

func TestRecruiterCanDownloadOnlyTheirApplicantsResume(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS063", NoResume: true})
	pdf := testutil.PDF()
	e.Upload(s, "/me/resume", "file", "cv.pdf", pdf)
	app := w.mustApply(s, e.Job(w.company.CompanyID, testutil.JobOpts{}))

	r := e.Get(w.company, "/company/applications/"+app+"/resume")
	if r.Status != 200 || !bytes.Equal(r.Body, pdf) {
		t.Fatalf("recruiter download: %d", r.Status)
	}
	detail := e.Get(w.company, "/company/applications/"+app).JSON()
	if detail["email"] != s.Email || detail["hasResume"] != true || len(detail["history"].([]any)) != 1 {
		t.Errorf("candidate detail = %v", detail)
	}
	if strings.Contains(detail["email"].(string), "hash") {
		t.Error("candidate detail leaks credentials")
	}
	if r := e.Get(e.Company("Rival Co", true), "/company/applications/"+app+"/resume"); r.Status != 404 {
		t.Errorf("rival recruiter downloading the resume: %d, want 404", r.Status)
	}
	if r := e.Get(e.Student(testutil.StudentOpts{Roll: "21CS064"}), "/company/applications/"+app+"/resume"); r.Status != 403 {
		t.Errorf("another student downloading it: %d, want 403", r.Status)
	}
}

func TestAdminCSVImportCreatesStudentsWithTemporaryPasswords(t *testing.T) {
	w := newWorld(t)
	e := w.e
	csv := "Name,Roll,Email,Branch,CGPA,Backlogs,Year,Phone\n" +
		"Ananya Sharma,21cs047,ananya@college.edu,CSE,8.6,0,Final year,+91 98765 43210\n" +
		"Rahul Verma,21it018,rahul@college.edu,IT,8.1,1,,\n" +
		"Bad Branch,21xx001,bad@college.edu,Astrology,7,0,,\n" +
		"Bad CGPA,21cs002,cgpa@college.edu,CSE,eleven,0,,\n" +
		"Dup Email,21cs003,ANANYA@college.edu,CSE,7,0,,\n" +
		"Dup In File,21CS047,other@college.edu,CSE,7,0,,\n" +
		"=cmd|' /C calc'!A0,21cs004,evil@college.edu,CSE,7,0,,\n"
	r := e.Upload(w.admin, "/admin/students/import", "file", "students.csv", []byte(csv))
	if r.Status != 200 {
		t.Fatalf("import: %d %s", r.Status, r.Body)
	}
	res := r.JSON()
	created := res["created"].([]any)
	errs := res["errors"].([]any)
	if len(created) != 3 { // Ananya, Rahul and the formula-named row (a valid name to the database)
		t.Fatalf("created %d, want 3: %s", len(created), r.Body)
	}
	if len(errs) != 4 {
		t.Fatalf("%d row errors, want 4 (branch, cgpa, duplicate email, duplicate roll): %v", len(errs), errs)
	}
	for _, x := range errs {
		if x.(map[string]any)["row"].(float64) < 2 {
			t.Errorf("row number counts from the header: %v", x)
		}
	}
	first := created[0].(map[string]any)
	if first["roll"] != "21CS047" || len(first["tempPassword"].(string)) < 10 {
		t.Errorf("created[0] = %v", first)
	}

	// The temporary password works once, and forces a change.
	rec := loginHTTP(e, "ananya@college.edu", first["tempPassword"].(string))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"mustChangePassword":true`) {
		t.Errorf("login with the temp password: %d %s", rec.Code, rec.Body)
	}
	// Re-importing the same file creates nothing new.
	again := e.Upload(w.admin, "/admin/students/import", "file", "students.csv", []byte(csv)).JSON()
	if len(again["created"].([]any)) != 0 {
		t.Errorf("re-import created duplicates: %v", again["created"])
	}
	// Missing required columns and non-admins.
	if r := e.Upload(w.admin, "/admin/students/import", "file", "x.csv", []byte("name,roll\nA,B\n")); r.Status != 422 {
		t.Errorf("missing columns: %d, want 422", r.Status)
	}
	if r := e.Upload(e.Student(testutil.StudentOpts{Roll: "21CS065"}), "/admin/students/import", "file", "x.csv", []byte(csv)); r.Status != 403 {
		t.Errorf("student importing: %d, want 403", r.Status)
	}
}

func TestAdminStudentListingSearchAndCSVExport(t *testing.T) {
	w := newWorld(t)
	e := w.e
	e.Student(testutil.StudentOpts{Roll: "21CS070", Name: "Ananya Sharma", CGPA: 8.6, Branch: "CSE"})
	e.Student(testutil.StudentOpts{Roll: "21IT071", Name: "Rahul Verma", CGPA: 8.1, Branch: "IT"})
	evil := e.Student(testutil.StudentOpts{Roll: "21EC072", Name: "=HYPERLINK(\"http://evil\")", CGPA: 7, Branch: "ECE"})
	_ = evil
	e.Student(testutil.StudentOpts{Roll: "21CE073", Name: "100% Legit_Name", CGPA: 7, Branch: "Civil"})

	list := func(q string) []map[string]any { return e.Get(w.admin, "/admin/students?"+q).Items() }
	if n := len(list("")); n != 4 {
		t.Errorf("all = %d", n)
	}
	if got := list("q=ananya"); len(got) != 1 || got[0]["roll"] != "21CS070" {
		t.Errorf("search by name = %v", got)
	}
	if got := list("q=21IT"); len(got) != 1 {
		t.Errorf("search by roll = %v", got)
	}
	if got := list("branch=ECE"); len(got) != 1 {
		t.Errorf("branch filter = %v", got)
	}
	if got := list("status=Unplaced"); len(got) != 4 {
		t.Errorf("status filter = %d", len(got))
	}
	// Wildcards are literal: "%" must not match everyone.
	if got := list("q=%25"); len(got) != 1 || got[0]["roll"] != "21CE073" {
		t.Errorf("q=%%25 matched %d students, want only the one containing a literal %%", len(got))
	}
	if got := list("q=_"); len(got) != 1 {
		t.Errorf("q=_ matched %d students, want only the one with a literal underscore", len(got))
	}
	if r := e.Get(w.admin, "/admin/students?status=Whatever"); r.Status != 422 {
		t.Errorf("bad status filter: %d, want 422", r.Status)
	}
	page := e.Get(w.admin, "/admin/students?limit=2&page=2").JSON()
	if len(page["items"].([]any)) != 2 || page["total"].(float64) != 4 || page["page"].(float64) != 2 {
		t.Errorf("pagination = %v", page)
	}

	// CSV export neutralises spreadsheet formulas.
	csv := e.Get(w.admin, "/admin/students/export.csv")
	if csv.Status != 200 || !strings.HasPrefix(csv.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("export: %d %s", csv.Status, csv.Header.Get("Content-Type"))
	}
	body := string(csv.Body)
	if strings.Contains(body, ",=HYPERLINK") || strings.Contains(body, "\n=HYPERLINK") {
		t.Errorf("CSV contains a live formula:\n%s", body)
	}
	if !strings.Contains(body, "'=HYPERLINK") {
		t.Errorf("formula was dropped instead of neutralised:\n%s", body)
	}
	if !strings.HasPrefix(body, "roll,name,email,branch,cgpa,status,company,ctc_lpa") {
		t.Errorf("header = %q", strings.SplitN(body, "\n", 2)[0])
	}
}

func TestAdminEditsAcademicsAndEligibilityFollows(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "21CS080", CGPA: 6.9, Branch: "CSE"})
	j := e.Job(w.company.CompanyID, testutil.JobOpts{MinCGPA: 7.0})
	if r := w.apply(s, j); r.ErrCode() != "cgpa_too_low" {
		t.Fatalf("precondition: %d %s", r.Status, r.Body)
	}
	if r := e.Req(w.admin, "PATCH", "/admin/students/"+s.StudentID.String(), map[string]any{"cgpa": 11}); r.Status != 422 {
		t.Errorf("cgpa 11 accepted: %d", r.Status)
	}
	if r := e.Req(w.admin, "PATCH", "/admin/students/"+s.StudentID.String(), map[string]any{"cgpa": 7.4, "backlogs": 0}); r.Status != 200 {
		t.Fatalf("admin update: %d %s", r.Status, r.Body)
	}
	if r := w.apply(s, j); r.Status != 201 {
		t.Errorf("apply after the CGPA update: %d %s", r.Status, r.Body)
	}
	if r := e.Req(s, "PATCH", "/admin/students/"+s.StudentID.String(), map[string]any{"cgpa": 10}); r.Status != 403 {
		t.Errorf("student editing academics: %d, want 403", r.Status)
	}
	// History and reset.
	if got := e.Get(w.admin, "/admin/students/"+s.StudentID.String()+"/applications").Items(); len(got) != 1 {
		t.Errorf("admin view of the student's history = %v", got)
	}
	rp := e.Post(w.admin, "/admin/students/"+s.StudentID.String()+"/reset-password", nil)
	if rp.Status != 200 || len(rp.JSON()["tempPassword"].(string)) < 10 {
		t.Fatalf("reset: %d %s", rp.Status, rp.Body)
	}
	if rec := loginHTTP(e, s.Email, testutil.Password); rec.Code != 401 {
		t.Errorf("old password still works after reset")
	}
	if rec := loginHTTP(e, s.Email, rp.JSON()["tempPassword"].(string)); rec.Code != 200 {
		t.Errorf("new temp password rejected: %d", rec.Code)
	}
	if n := e.Count(`SELECT count(*) FROM audit_log WHERE action = 'student.password_reset'`); n != 1 {
		t.Errorf("password reset not audited")
	}
}

func TestStudentsCannotReachEachOthersData(t *testing.T) {
	w := newWorld(t)
	e := w.e
	a := e.Student(testutil.StudentOpts{Roll: "21CS081"})
	b := e.Student(testutil.StudentOpts{Roll: "21CS082"})
	appA := w.mustApply(a, e.Job(w.company.CompanyID, testutil.JobOpts{}))
	if got := e.Get(b, "/me/applications").Items(); len(got) != 0 {
		t.Errorf("student B sees A's applications: %v", got)
	}
	if r := e.Post(b, "/me/applications/"+appA+"/withdraw", nil); r.Status != 404 {
		t.Errorf("B withdrawing A's application: %d", r.Status)
	}
	// There is no route that takes a student id for the student's own data.
	if r := e.Get(b, "/me/profile?id="+a.StudentID.String()); r.JSON()["roll"] != "21CS082" {
		t.Errorf("profile followed a client-supplied id")
	}
	if r := e.Get(b, "/admin/students/"+a.StudentID.String()); r.Status != http.StatusForbidden {
		t.Errorf("student reading the admin student view: %d, want 403", r.Status)
	}
}
