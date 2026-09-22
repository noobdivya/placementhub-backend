package app_test

import (
	"encoding/json"
	"testing"

	"placementhub/internal/testutil"
)

func TestAdminCanAddAnotherPlacementOfficer(t *testing.T) {
	w := newWorld(t)
	e := w.e

	r := e.Post(w.admin, "/admin/admins", map[string]any{"name": "Ms. Second Officer", "email": "Officer2@College.edu"})
	if r.Status != 201 {
		t.Fatalf("create admin: %d %s", r.Status, r.Body)
	}
	temp := r.JSON()["tempPassword"].(string)
	if r.JSON()["user"].(map[string]any)["role"] != "admin" {
		t.Errorf("created user = %v", r.JSON()["user"])
	}
	if r := e.Post(w.admin, "/admin/admins", map[string]any{"name": "Dup", "email": "officer2@college.edu"}); r.Status != 409 {
		t.Errorf("duplicate admin: %d, want 409", r.Status)
	}
	if r := e.Post(w.admin, "/admin/admins", map[string]any{"name": "x", "email": "nope"}); r.Status != 422 {
		t.Errorf("invalid admin: %d, want 422", r.Status)
	}
	// Only admins can create admins.
	if r := e.Post(w.company, "/admin/admins", map[string]any{"name": "Evil Admin", "email": "evil@x.co"}); r.Status != 403 {
		t.Errorf("company creating an admin: %d, want 403", r.Status)
	}
	if r := e.Post(e.Student(testutil.StudentOpts{Roll: "21CS110"}), "/admin/admins", map[string]any{"name": "Evil Admin", "email": "evil2@x.co"}); r.Status != 403 {
		t.Errorf("student creating an admin: %d, want 403", r.Status)
	}

	// The new officer must change the temporary password before doing anything.
	rec := loginHTTP(e, "officer2@college.edu", temp)
	var sess struct {
		AccessToken string `json:"accessToken"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	officer := &testutil.User{Token: sess.AccessToken}
	if r := e.Get(officer, "/admin/students"); r.Status != 403 || r.ErrCode() != "password_change_required" {
		t.Errorf("admin routes with a temp password: %d %s", r.Status, r.Body)
	}
	ch := e.Post(officer, "/auth/change-password", map[string]any{"currentPassword": temp, "newPassword": "Officer-passw0rd!"})
	if ch.Status != 200 {
		t.Fatalf("change password: %d %s", ch.Status, ch.Body)
	}
	officer.Token = ch.JSON()["accessToken"].(string)
	if r := e.Get(officer, "/admin/students"); r.Status != 200 {
		t.Errorf("admin routes after changing the password: %d", r.Status)
	}
	if n := e.Count(`SELECT count(*) FROM audit_log WHERE action = 'admin.created'`); n != 1 {
		t.Errorf("admin creation not audited")
	}
}

func TestAdminCanResetARecruitersPassword(t *testing.T) {
	w := newWorld(t)
	e := w.e
	before := refreshCookie(loginHTTP(e, w.company.Email, testutil.Password))

	if r := e.Post(w.company, "/admin/companies/"+w.company.CompanyID.String()+"/reset-password", nil); r.Status != 403 {
		t.Errorf("a company resetting its own password through the admin route: %d, want 403", r.Status)
	}
	r := e.Post(w.admin, "/admin/companies/"+w.company.CompanyID.String()+"/reset-password", nil)
	if r.Status != 200 {
		t.Fatalf("reset: %d %s", r.Status, r.Body)
	}
	temp := r.JSON()["tempPassword"].(string)
	if rec := loginHTTP(e, w.company.Email, testutil.Password); rec.Code != 401 {
		t.Errorf("the old password still works")
	}
	if rec := raw(e, "POST", "/auth/refresh", "", nil, before); rec.Code != 401 {
		t.Errorf("an old session survived the reset: %d", rec.Code)
	}
	if rec := loginHTTP(e, w.company.Email, temp); rec.Code != 200 {
		t.Errorf("the temporary password does not work: %d", rec.Code)
	}
	if r := e.Post(w.admin, "/admin/companies/00000000-0000-0000-0000-000000000000/reset-password", nil); r.Status != 404 {
		t.Errorf("unknown company: %d, want 404", r.Status)
	}
}
