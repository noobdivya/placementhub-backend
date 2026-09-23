package app_test

import (
	"testing"

	"placementhub/internal/testutil"

	"github.com/google/uuid"
)

// postJobWithRound creates a Draft job, gives it one named round, and returns
// the job id.
func (w *world) postJobWithRound(roundName string) string {
	w.e.T.Helper()
	deadline := w.e.Clock.Now().In(w.e.Cfg.Location).AddDate(0, 0, 15).Format("2006-01-02")
	created := w.e.Post(w.company, "/company/jobs", jobBody(deadline, nil))
	if created.Status != 201 {
		w.e.T.Fatalf("create job: %d %s", created.Status, created.Body)
	}
	id := created.JSON()["id"].(string)
	r := w.e.Req(w.company, "PUT", "/company/jobs/"+id+"/rounds", map[string]any{
		"items": []map[string]any{{"name": roundName, "mode": "Online", "durationMinutes": 60}},
	})
	if r.Status != 200 {
		w.e.T.Fatalf("define round: %d %s", r.Status, r.Body)
	}
	return id
}

func TestRoundsRequiredToSubmit(t *testing.T) {
	w := newWorld(t)
	e := w.e
	deadline := e.Clock.Now().In(e.Cfg.Location).AddDate(0, 0, 15).Format("2006-01-02")
	created := e.Post(w.company, "/company/jobs", jobBody(deadline, nil))
	id := created.JSON()["id"].(string)

	if r := e.Post(w.company, "/company/jobs/"+id+"/submit", nil); r.Status != 422 || r.ErrCode() != "rounds_required" {
		t.Fatalf("submit without rounds: %d %s", r.Status, r.Body)
	}
	w.defineRound(id)
	if r := e.Post(w.company, "/company/jobs/"+id+"/submit", nil); r.Status != 200 {
		t.Fatalf("submit with a round: %d %s", r.Status, r.Body)
	}
}

func TestStudentSeesRoundsBeforeAndAfterApplying(t *testing.T) {
	w := newWorld(t)
	e := w.e
	id := w.postJobWithRound("Aptitude Test")
	e.Post(w.company, "/company/jobs/"+id+"/submit", nil)
	w.approve(id)

	s := e.Student(testutil.StudentOpts{Roll: "21CS001"})
	before := e.Get(s, "/jobs/"+id+"/rounds").Items()
	if len(before) != 1 || before[0]["name"] != "Aptitude Test" {
		t.Fatalf("rounds before applying = %v", before)
	}

	appID := w.mustApply(s, uuid.MustParse(id))
	after := e.Get(s, "/me/applications/"+appID+"/rounds").Items()
	if len(after) != 1 || after[0]["status"] != "Upcoming" || after[0]["current"] != true {
		t.Fatalf("rounds after applying = %v", after)
	}
}

func TestReplaceReordersFreelyBeforeProgress(t *testing.T) {
	w := newWorld(t)
	e := w.e
	id := w.postJobWithRound("Aptitude Test")

	r := e.Req(w.company, "PUT", "/company/jobs/"+id+"/rounds", map[string]any{
		"items": []map[string]any{
			{"name": "Coding Round", "mode": "Online", "durationMinutes": 90},
			{"name": "HR Interview", "mode": "Offline", "durationMinutes": 30},
		},
	})
	if r.Status != 200 {
		t.Fatalf("replace: %d %s", r.Status, r.Body)
	}
	items := r.Items()
	if len(items) != 2 || items[0]["name"] != "Coding Round" || items[1]["name"] != "HR Interview" {
		t.Fatalf("rounds after replace = %v", items)
	}
}

func TestRoundLockedOnceCandidateInProgress(t *testing.T) {
	w := newWorld(t)
	e := w.e
	id := w.postJobWithRound("Aptitude Test")
	e.Post(w.company, "/company/jobs/"+id+"/submit", nil)
	w.approve(id)

	s := e.Student(testutil.StudentOpts{Roll: "21CS002"})
	appID := w.mustApply(s, uuid.MustParse(id))

	rounds := e.Get(w.company, "/company/jobs/"+id+"/rounds").Items()
	roundID := rounds[0]["id"].(string)

	// Scheduling it locks identity/removal, but not its own logistics.
	if r := e.Req(w.company, "PATCH", "/company/applications/"+appID+"/rounds/"+roundID, map[string]any{"status": "Scheduled"}); r.Status != 200 {
		t.Fatalf("set status: %d %s", r.Status, r.Body)
	}

	if r := e.Req(w.company, "PUT", "/company/jobs/"+id+"/rounds", map[string]any{"items": []map[string]any{
		{"name": "Renamed", "mode": "Online", "durationMinutes": 60},
	}}); r.Status != 409 || r.ErrCode() != "rounds_locked" {
		t.Fatalf("rename a locked round: %d %s", r.Status, r.Body)
	}
	if r := e.Req(w.company, "PUT", "/company/jobs/"+id+"/rounds", map[string]any{"items": []map[string]any{}}); r.Status != 422 {
		t.Fatalf("removing every round: %d %s", r.Status, r.Body)
	}

	// Same name, same order, new schedule/location: allowed.
	if r := e.Req(w.company, "PUT", "/company/jobs/"+id+"/rounds", map[string]any{"items": []map[string]any{
		{"id": roundID, "name": "Aptitude Test", "mode": "Offline", "durationMinutes": 60, "location": "Room 12"},
	}}); r.Status != 200 {
		t.Fatalf("reschedule a locked round: %d %s", r.Status, r.Body)
	}
}

func TestRoundStatusTransitionsAndGuards(t *testing.T) {
	w := newWorld(t)
	e := w.e
	id := w.postJobWithRound("Aptitude Test")
	e.Post(w.company, "/company/jobs/"+id+"/submit", nil)
	w.approve(id)

	s := e.Student(testutil.StudentOpts{Roll: "21CS003"})
	appID := w.mustApply(s, uuid.MustParse(id))
	roundID := e.Get(w.company, "/company/jobs/"+id+"/rounds").Items()[0]["id"].(string)

	// No skipping Upcoming -> Cleared.
	if r := e.Req(w.company, "PATCH", "/company/applications/"+appID+"/rounds/"+roundID, map[string]any{"status": "Cleared"}); r.Status != 422 || r.ErrCode() != "invalid_transition" {
		t.Fatalf("skip to Cleared: %d %s", r.Status, r.Body)
	}
	if r := e.Req(w.company, "PATCH", "/company/applications/"+appID+"/rounds/"+roundID, map[string]any{"status": "Scheduled"}); r.Status != 200 {
		t.Fatalf("schedule: %d %s", r.Status, r.Body)
	}
	if r := e.Req(w.company, "PATCH", "/company/applications/"+appID+"/rounds/"+roundID, map[string]any{"status": "Cleared"}); r.Status != 200 || r.JSON()["status"] != "Cleared" {
		t.Fatalf("clear: %d %s", r.Status, r.Body)
	}

	// Withdrawn application: no further round changes.
	w.e.Post(s, "/me/applications/"+appID+"/withdraw", nil)
	if r := e.Req(w.company, "PATCH", "/company/applications/"+appID+"/rounds/"+roundID, map[string]any{"status": "Scheduled"}); r.Status != 409 || r.ErrCode() != "application_withdrawn" {
		t.Fatalf("round change on withdrawn application: %d %s", r.Status, r.Body)
	}
}

func TestRoundStatusBlockedOnceOfferAccepted(t *testing.T) {
	w := newWorld(t)
	e := w.e
	id := w.postJobWithRound("Aptitude Test")
	e.Post(w.company, "/company/jobs/"+id+"/submit", nil)
	w.approve(id)

	s := e.Student(testutil.StudentOpts{Roll: "21CS004"})
	appID := w.mustApply(s, uuid.MustParse(id))
	roundID := e.Get(w.company, "/company/jobs/"+id+"/rounds").Items()[0]["id"].(string)

	offerID := w.offer(appID) // Applied -> Shortlisted -> Interview -> Offered
	if r := w.accept(s, offerID); r.Status != 200 {
		t.Fatalf("accept: %d %s", r.Status, r.Body)
	}
	if r := e.Req(w.company, "PATCH", "/company/applications/"+appID+"/rounds/"+roundID, map[string]any{"status": "Scheduled"}); r.Status != 409 || r.ErrCode() != "offer_accepted" {
		t.Fatalf("round change after offer accepted: %d %s", r.Status, r.Body)
	}
}
