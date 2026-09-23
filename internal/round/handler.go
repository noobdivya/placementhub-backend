package round

import (
	"net/http"

	"placementhub/internal/auth"
	"placementhub/internal/config"
	"placementhub/internal/httpx"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Handler struct {
	svc *Service
	cfg config.Config
}

func NewHandler(svc *Service, cfg config.Config) *Handler { return &Handler{svc: svc, cfg: cfg} }

// StudentJobRoutes adds GET /{id}/rounds next to the student job listing
// (mounted under /jobs, behind RequireRole(student)).
func (h *Handler) StudentJobRoutes(r chi.Router) {
	r.Get("/{id}/rounds", httpx.Handle(h.forJob))
}

// MeRoutes adds GET /applications/{id}/rounds under /me (student).
func (h *Handler) MeRoutes(r chi.Router) {
	r.Get("/applications/{id}/rounds", httpx.Handle(h.forApplication))
}

// CompanyJobRoutes are mounted at /company/jobs/{id}/rounds behind RequireRole(company).
func (h *Handler) CompanyJobRoutes(r chi.Router) {
	r.Get("/{id}/rounds", httpx.Handle(h.forCompanyJob))
	r.Put("/{id}/rounds", httpx.Handle(h.replace))
}

// CompanyRoutes adds the per-application round endpoints under /company (company).
func (h *Handler) CompanyRoutes(r chi.Router) {
	r.Get("/applications/{id}/rounds", httpx.Handle(h.forCompanyApplication))
	r.Patch("/applications/{id}/rounds/{roundId}", httpx.Handle(h.setStatus))
}

func (h *Handler) uid(r *http.Request) uuid.UUID { return auth.MustPrincipal(r).UserID }

func idParam(r *http.Request, name, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, httpx.NotFound(what)
	}
	return id, nil
}

func (h *Handler) forJob(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "id", "job")
	if err != nil {
		return err
	}
	items, err := h.svc.ForJob(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (h *Handler) forApplication(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "id", "application")
	if err != nil {
		return err
	}
	items, err := h.svc.ForApplication(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (h *Handler) forCompanyJob(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "id", "job")
	if err != nil {
		return err
	}
	items, err := h.svc.ForCompanyJob(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (h *Handler) replace(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "id", "job")
	if err != nil {
		return err
	}
	var in struct {
		Items []RoundInput `json:"items"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	items, err := h.svc.Replace(r.Context(), h.uid(r), id, in.Items)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (h *Handler) forCompanyApplication(w http.ResponseWriter, r *http.Request) error {
	id, err := idParam(r, "id", "application")
	if err != nil {
		return err
	}
	items, err := h.svc.ForCompanyApplication(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request) error {
	appID, err := idParam(r, "id", "application")
	if err != nil {
		return err
	}
	roundID, err := idParam(r, "roundId", "round")
	if err != nil {
		return err
	}
	var in StatusChange
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	p, err := h.svc.SetStatus(r.Context(), h.uid(r), appID, roundID, in)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, p)
	return nil
}
