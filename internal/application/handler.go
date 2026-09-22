package application

import (
	"net/http"

	"placementhub/internal/auth"
	"placementhub/internal/config"
	"placementhub/internal/domain"
	"placementhub/internal/httpx"
	"placementhub/internal/student"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Handler struct {
	svc      *Service
	students *student.Service
	cfg      config.Config
}

func NewHandler(svc *Service, students *student.Service, cfg config.Config) *Handler {
	return &Handler{svc: svc, students: students, cfg: cfg}
}

// MeRoutes are mounted at /me behind RequireRole(student).
func (h *Handler) MeRoutes(r chi.Router) {
	r.Get("/applications", httpx.Handle(h.listMine))
	r.Post("/applications/{id}/withdraw", httpx.Handle(h.withdraw))
	r.Get("/offers", httpx.Handle(h.listOffers))
	r.Post("/offers/{id}/accept", httpx.Handle(h.accept))
	r.Post("/offers/{id}/decline", httpx.Handle(h.decline))
}

// JobRoutes adds POST /jobs/{id}/apply next to the student job listing.
func (h *Handler) JobRoutes(r chi.Router) {
	r.Post("/{id}/apply", httpx.Handle(h.apply))
}

// CompanyRoutes are mounted at /company behind RequireRole(company).
func (h *Handler) CompanyRoutes(r chi.Router) {
	r.Get("/candidates", httpx.Handle(h.candidates))
	r.Get("/applications/{id}", httpx.Handle(h.candidateDetail))
	r.Patch("/applications/{id}/stage", httpx.Handle(h.moveStage))
	r.Get("/applications/{id}/resume", httpx.Handle(h.candidateResume))
}

// AdminRoutes are mounted at /admin behind RequireRole(admin).
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Post("/offers/{id}/revoke", httpx.Handle(h.revoke))
}

// HistoryHandler serves GET /admin/students/{id}/applications; it is mounted
// inside the students router by the app wiring.
func (h *Handler) HistoryHandler() http.HandlerFunc { return httpx.Handle(h.studentHistory) }

func (h *Handler) uid(r *http.Request) uuid.UUID { return auth.MustPrincipal(r).UserID }

func idParam(r *http.Request, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, httpx.NotFound(what)
	}
	return id, nil
}

func (h *Handler) apply(w http.ResponseWriter, r *http.Request) error {
	jobID, err := idParam(r, "job")
	if err != nil {
		return err
	}
	a, err := h.svc.Apply(r.Context(), h.uid(r), jobID)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, a)
	return nil
}

func (h *Handler) listMine(w http.ResponseWriter, r *http.Request) error {
	apps, err := h.svc.ListMine(r.Context(), h.uid(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": apps})
	return nil
}

func (h *Handler) withdraw(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "application")
	if err != nil {
		return err
	}
	a, err := h.svc.Withdraw(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, a)
	return nil
}

func (h *Handler) listOffers(w http.ResponseWriter, r *http.Request) error {
	offers, err := h.svc.ListOffers(r.Context(), h.uid(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": offers})
	return nil
}

func (h *Handler) accept(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "offer")
	if err != nil {
		return err
	}
	res, err := h.svc.Accept(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, res)
	return nil
}

func (h *Handler) decline(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "offer")
	if err != nil {
		return err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
	}
	o, err := h.svc.Decline(r.Context(), h.uid(r), id, in.Reason)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, o)
	return nil
}

func (h *Handler) candidates(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	var jobID *uuid.UUID
	if s := q.Get("jobId"); s != "" && s != "all" {
		id, err := uuid.Parse(s)
		if err != nil {
			return httpx.Unprocessable("bad_filter", "jobId must be a job id")
		}
		jobID = &id
	}
	stage := q.Get("stage")
	if stage != "" {
		var v httpx.V
		v.OneOf("stage", stage, domain.Stages...)
		if err := v.Err(); err != nil {
			return err
		}
	}
	cs, err := h.svc.ListCandidates(r.Context(), h.uid(r), jobID, stage, q.Get("includeWithdrawn") == "true")
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": cs})
	return nil
}

func (h *Handler) candidateDetail(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "application")
	if err != nil {
		return err
	}
	c, err := h.svc.CandidateDetail(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, c)
	return nil
}

func (h *Handler) moveStage(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "application")
	if err != nil {
		return err
	}
	var in StageChange
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	c, err := h.svc.MoveStage(r.Context(), h.uid(r), id, in)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, c)
	return nil
}

func (h *Handler) candidateResume(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "application")
	if err != nil {
		return err
	}
	sid, err := h.svc.StudentForCandidate(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	return student.ServeResume(w, r, h.students, sid)
}

func (h *Handler) studentHistory(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "student")
	if err != nil {
		return err
	}
	apps, err := h.svc.ListForStudent(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": apps})
	return nil
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "offer")
	if err != nil {
		return err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	o, err := h.svc.RevokePlacement(r.Context(), id, in.Reason, h.uid(r), httpx.ClientIP(r, h.cfg.TrustedProxy))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, o)
	return nil
}
