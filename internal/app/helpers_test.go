package app_test

import (
	"net/http"
	"testing"

	"placementhub/internal/testutil"

	"github.com/google/uuid"
)

// pipeline drives an application through the recruiter's stages via the API.
type world struct {
	e       *testutil.Env
	admin   *testutil.User
	company *testutil.User
}

func newWorld(t *testing.T) *world {
	t.Helper()
	e := testutil.NewEnv(t)
	return &world{e: e, admin: e.Admin(), company: e.Company("Nimbus Labs", true)}
}

func (w *world) apply(s *testutil.User, jobID uuid.UUID) testutil.Resp {
	w.e.T.Helper()
	return w.e.Post(s, "/jobs/"+jobID.String()+"/apply", nil)
}

func (w *world) mustApply(s *testutil.User, jobID uuid.UUID) string {
	w.e.T.Helper()
	r := w.apply(s, jobID)
	if r.Status != http.StatusCreated {
		w.e.T.Fatalf("apply: %d %s", r.Status, r.Body)
	}
	return r.JSON()["id"].(string)
}

func (w *world) move(appID, stage string, extra map[string]any) testutil.Resp {
	w.e.T.Helper()
	body := map[string]any{"stage": stage}
	for k, v := range extra {
		body[k] = v
	}
	return w.e.Req(w.company, "PATCH", "/company/applications/"+appID+"/stage", body)
}

func (w *world) mustMove(appID, stage string) map[string]any {
	w.e.T.Helper()
	r := w.move(appID, stage, nil)
	if r.Status != http.StatusOK {
		w.e.T.Fatalf("move to %s: %d %s", stage, r.Status, r.Body)
	}
	return r.JSON()
}

// offer takes an application Applied -> Shortlisted -> Interview -> Offered and
// returns the resulting offer id.
func (w *world) offer(appID string) string {
	w.e.T.Helper()
	w.mustMove(appID, "Shortlisted")
	w.mustMove(appID, "Interview")
	c := w.mustMove(appID, "Offered")
	offer, ok := c["offer"].(map[string]any)
	if !ok {
		w.e.T.Fatalf("no offer on candidate: %v", c)
	}
	return offer["id"].(string)
}

func (w *world) accept(s *testutil.User, offerID string) testutil.Resp {
	w.e.T.Helper()
	return w.e.Post(s, "/me/offers/"+offerID+"/accept", nil)
}

func (w *world) placementStatus(s *testutil.User) string {
	w.e.T.Helper()
	r := w.e.Get(s, "/me/profile")
	if r.Status != http.StatusOK {
		w.e.T.Fatalf("profile: %d %s", r.Status, r.Body)
	}
	return r.JSON()["placement"].(map[string]any)["status"].(string)
}

func (w *world) stageOf(s *testutil.User, appID string) string {
	w.e.T.Helper()
	for _, a := range w.e.Get(s, "/me/applications").Items() {
		if a["id"] == appID {
			return a["stage"].(string)
		}
	}
	w.e.T.Fatalf("application %s not in history", appID)
	return ""
}
