package app_test

import (
	"net/http"
	"testing"
	"time"

	"placementhub/internal/testutil"
)

// driveBody builds a drive `in` from now, in the college timezone.
func (w *world) driveBody(in time.Duration, over map[string]any) map[string]any {
	at := w.e.Clock.Now().In(w.e.Cfg.Location).Add(in)
	b := map[string]any{
		"companyId": w.company.CompanyID, "title": "Pool Campus Drive", "date": at.Format("2006-01-02"),
		"time": at.Format("15:04"), "mode": "On-campus", "venue": "Main Auditorium",
	}
	for k, v := range over {
		b[k] = v
	}
	return b
}

func (w *world) createDrive(in time.Duration, over map[string]any) map[string]any {
	w.e.T.Helper()
	r := w.e.Post(w.admin, "/admin/drives", w.driveBody(in, over))
	if r.Status != http.StatusCreated {
		w.e.T.Fatalf("create drive: %d %s", r.Status, r.Body)
	}
	return r.JSON()
}

func TestDriveCreationNotifiesOnlyEligibleStudentsAndCountsThem(t *testing.T) {
	w := newWorld(t)
	e := w.e
	eligible := e.Student(testutil.StudentOpts{Roll: "D1", CGPA: 8.5, Branch: "CSE"})
	lowCGPA := e.Student(testutil.StudentOpts{Roll: "D2", CGPA: 6.5, Branch: "CSE"})
	wrongBranch := e.Student(testutil.StudentOpts{Roll: "D3", CGPA: 9, Branch: "Civil"})
	placed := e.Student(testutil.StudentOpts{Roll: "D4", CGPA: 9, Branch: "CSE"})
	w.placeStudent(placed)

	d := w.createDrive(48*time.Hour, map[string]any{"minCgpa": 8.0, "branches": []string{"CSE", "IT"}})
	if d["eligible"].(float64) != 1 || d["registered"].(float64) != 0 || d["status"] != "Upcoming" {
		t.Errorf("drive = eligible %v registered %v status %v", d["eligible"], d["registered"], d["status"])
	}
	got := w.notifiedUsers("drive_new")
	if !got[eligible.ID] || got[lowCGPA.ID] || got[wrongBranch.ID] || got[placed.ID] || len(got) != 1 {
		t.Errorf("drive_new recipients = %v, want only the eligible student", got)
	}
	// Displayed in the frontend's Drive shape.
	if d["company"] != "Nimbus Labs" || d["mode"] != "On-campus" || d["venue"] != "Main Auditorium" || d["date"] == nil || d["time"] == nil {
		t.Errorf("drive JSON = %v", d)
	}
}

func TestDriveRegistrationRules(t *testing.T) {
	w := newWorld(t)
	e := w.e
	ok := e.Student(testutil.StudentOpts{Roll: "D10", CGPA: 8.5, Branch: "CSE"})
	low := e.Student(testutil.StudentOpts{Roll: "D11", CGPA: 6.5, Branch: "CSE"})
	d := w.createDrive(48*time.Hour, map[string]any{"minCgpa": 8.0, "branches": []string{"CSE"}})
	id := d["id"].(string)

	list := e.Get(ok, "/drives").Items()
	if len(list) != 1 || list[0]["canRegister"] != true || list[0]["isRegistered"] != false {
		t.Fatalf("student drive list = %v", list)
	}
	if r := e.Post(low, "/drives/"+id+"/register", nil); r.Status != 422 || r.ErrCode() != "cgpa_too_low" {
		t.Errorf("ineligible registration: %d %s", r.Status, r.Body)
	}
	if r := e.Post(ok, "/drives/"+id+"/register", nil); r.Status != 200 || r.JSON()["isRegistered"] != true {
		t.Fatalf("register: %d %s", r.Status, r.Body)
	}
	if r := e.Post(ok, "/drives/"+id+"/register", nil); r.Status != 200 { // idempotent
		t.Errorf("registering twice: %d", r.Status)
	}
	if n := e.Count(`SELECT count(*) FROM drive_registrations WHERE drive_id = $1`, id); n != 1 {
		t.Errorf("%d registrations, want 1", n)
	}
	// Admin sees the roster and counts.
	roster := e.Get(w.admin, "/admin/drives/"+id+"/registrations").Items()
	if len(roster) != 1 || roster[0]["roll"] != "D10" {
		t.Errorf("roster = %v", roster)
	}
	if got := e.Get(w.admin, "/admin/drives/"+id).JSON()["registered"]; got.(float64) != 1 {
		t.Errorf("registered count = %v", got)
	}
	// Unregister.
	if r := e.Req(ok, "DELETE", "/drives/"+id+"/register", nil); r.Status != 204 {
		t.Errorf("unregister: %d", r.Status)
	}
	if r := e.Req(ok, "DELETE", "/drives/"+id+"/register", nil); r.Status != 404 {
		t.Errorf("unregister when not registered: %d, want 404", r.Status)
	}
	// Students cannot manage drives.
	if r := e.Post(ok, "/admin/drives", w.driveBody(time.Hour, nil)); r.Status != 403 {
		t.Errorf("student creating a drive: %d, want 403", r.Status)
	}
	if r := e.Req(ok, "DELETE", "/admin/drives/"+id, nil); r.Status != 403 {
		t.Errorf("student deleting a drive: %d, want 403", r.Status)
	}
}

func TestDriveStatusFollowsTheClockAndClosesRegistration(t *testing.T) {
	w := newWorld(t)
	e := w.e
	s := e.Student(testutil.StudentOpts{Roll: "D20"})
	d := w.createDrive(2*time.Hour, map[string]any{"durationMinutes": 60})
	id := d["id"].(string)
	status := func() string {
		e.Relogin(w.admin) // the clock jumps far past the access-token lifetime
		for _, x := range e.Get(w.admin, "/admin/drives").Items() {
			if x["id"] == id {
				return x["status"].(string)
			}
		}
		return "?"
	}
	if status() != "Upcoming" {
		t.Fatalf("status = %s", status())
	}
	e.Clock.Advance(2*time.Hour + 10*time.Minute)
	if status() != "Ongoing" {
		t.Errorf("status during the drive = %s, want Ongoing", status())
	}
	e.Relogin(s)
	if r := e.Post(s, "/drives/"+id+"/register", nil); r.Status != 422 || r.ErrCode() != "drive_started" {
		t.Errorf("registering after start: %d %s", r.Status, r.Body)
	}
	e.Clock.Advance(2 * time.Hour)
	if status() != "Completed" {
		t.Errorf("status after the drive = %s, want Completed", status())
	}
	e.Relogin(s)
	if n := len(e.Get(s, "/drives").Items()); n != 0 {
		t.Errorf("a finished drive is in the upcoming list")
	}
	if n := len(e.Get(s, "/drives?scope=past").Items()); n != 1 {
		t.Errorf("past scope returned %d, want 1", n)
	}
	e.Relogin(w.admin)
	if got := e.Get(w.admin, "/admin/drives?status=Completed").Items(); len(got) != 1 {
		t.Errorf("status filter returned %d", len(got))
	}
}

func TestDriveUpdatesNotifyRegisteredStudentsOnlyWhenLogisticsChange(t *testing.T) {
	w := newWorld(t)
	e := w.e
	registered := e.Student(testutil.StudentOpts{Roll: "D30"})
	bystander := e.Student(testutil.StudentOpts{Roll: "D31"})
	d := w.createDrive(72*time.Hour, nil)
	id := d["id"].(string)
	e.Post(registered, "/drives/"+id+"/register", nil)

	// A cosmetic edit tells nobody.
	e.Req(w.admin, "PUT", "/admin/drives/"+id, w.driveBody(72*time.Hour, map[string]any{"title": "Pool Campus Drive 2026"}))
	if n := e.Count(`SELECT count(*) FROM notifications WHERE type = 'drive_updated'`); n != 0 {
		t.Errorf("%d update notifications for a title-only change", n)
	}
	// Moving the venue or time does, to registered students only.
	r := e.Req(w.admin, "PUT", "/admin/drives/"+id, w.driveBody(72*time.Hour, map[string]any{"venue": "Seminar Hall B"}))
	if r.Status != 200 {
		t.Fatalf("update: %d %s", r.Status, r.Body)
	}
	got := w.notifiedUsers("drive_updated")
	if !got[registered.ID] || got[bystander.ID] || len(got) != 1 {
		t.Errorf("update recipients = %v", got)
	}
	// Cancelling a drive tells registrants and removes it.
	if r := e.Req(w.admin, "DELETE", "/admin/drives/"+id, nil); r.Status != 204 {
		t.Fatalf("delete: %d", r.Status)
	}
	if n := e.Count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND dedupe_key = $2`, registered.ID, "drive_cancel:"+id); n != 1 {
		t.Errorf("cancellation not announced")
	}
	if n := e.Count(`SELECT count(*) FROM drive_registrations WHERE drive_id = $1`, id); n != 0 {
		t.Errorf("registrations survived the drive")
	}
}

func TestDriveInputValidation(t *testing.T) {
	w := newWorld(t)
	e := w.e
	other := e.Company("Quantra", true)
	jobOfOther := e.Job(other.CompanyID, testutil.JobOpts{})
	for name, body := range map[string]map[string]any{
		"start in the past":      w.driveBody(-2*time.Hour, nil),
		"bad mode":               w.driveBody(time.Hour, map[string]any{"mode": "Telepathy"}),
		"bad date":               w.driveBody(time.Hour, map[string]any{"date": "tomorrow"}),
		"bad time":               w.driveBody(time.Hour, map[string]any{"time": "25:99"}),
		"no title":               w.driveBody(time.Hour, map[string]any{"title": ""}),
		"unknown company":        w.driveBody(time.Hour, map[string]any{"companyId": "00000000-0000-0000-0000-000000000000"}),
		"job of another company": w.driveBody(time.Hour, map[string]any{"jobId": jobOfOther}),
		"bad cgpa":               w.driveBody(time.Hour, map[string]any{"minCgpa": 11}),
		"unknown branch":         w.driveBody(time.Hour, map[string]any{"branches": []string{"Astrology"}}),
	} {
		if r := e.Post(w.admin, "/admin/drives", body); r.Status != 422 {
			t.Errorf("%s: %d %s, want 422", name, r.Status, r.Body)
		}
	}
	// A linked job supplies the eligibility criteria.
	j := e.Job(w.company.CompanyID, testutil.JobOpts{MinCGPA: 8.2, Branches: []string{"IT"}})
	d := w.createDrive(24*time.Hour, map[string]any{"jobId": j})
	if d["minCgpa"].(float64) != 8.2 || len(d["branches"].([]any)) != 1 {
		t.Errorf("criteria not copied from the job: %v", d)
	}
}

func TestDriveRemindersGoToRegisteredStudentsOnce(t *testing.T) {
	w := newWorld(t)
	e := w.e
	registered := e.Student(testutil.StudentOpts{Roll: "D40"})
	other := e.Student(testutil.StudentOpts{Roll: "D41"})
	d := w.createDrive(30*time.Hour, nil)
	id := d["id"].(string)
	e.Post(registered, "/drives/"+id+"/register", nil)

	w.runScheduler()
	if n := e.Count(`SELECT count(*) FROM notifications WHERE type = 'drive_reminder'`); n != 0 {
		t.Fatalf("reminder sent 30h ahead")
	}
	e.Clock.Advance(10 * time.Hour) // now 20h ahead
	w.runScheduler()
	w.runScheduler()
	got := w.notifiedUsers("drive_reminder")
	if !got[registered.ID] || got[other.ID] || len(got) != 1 {
		t.Errorf("reminder recipients = %v", got)
	}
	if n := e.Count(`SELECT count(*) FROM notifications WHERE type = 'drive_reminder'`); n != 1 {
		t.Errorf("%d reminders, want 1", n)
	}
}
