package drive

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

// StudentRoutes are mounted at /drives behind RequireRole(student).
func (h *Handler) StudentRoutes(r chi.Router) {
	r.Get("/", httpx.Handle(h.studentList))
	r.Post("/{id}/register", httpx.Handle(h.register))
	r.Delete("/{id}/register", httpx.Handle(h.unregister))
}

// AdminRoutes are mounted at /admin/drives behind RequireRole(admin).
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Get("/", httpx.Handle(h.adminList))
	r.Post("/", httpx.Handle(h.adminCreate))
	r.Get("/{id}", httpx.Handle(h.adminGet))
	r.Put("/{id}", httpx.Handle(h.adminUpdate))
	r.Delete("/{id}", httpx.Handle(h.adminDelete))
	r.Get("/{id}/registrations", httpx.Handle(h.adminRegistrations))
}

func (h *Handler) uid(r *http.Request) uuid.UUID { return auth.MustPrincipal(r).UserID }
func (h *Handler) ip(r *http.Request) string     { return httpx.ClientIP(r, h.cfg.TrustedProxy) }

func driveID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, httpx.NotFound("drive")
	}
	return id, nil
}

func (h *Handler) studentList(w http.ResponseWriter, r *http.Request) error {
	past := r.URL.Query().Get("scope") == "past"
	ds, err := h.svc.ListForStudent(r.Context(), h.uid(r), past)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": ds})
	return nil
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) error {
	id, err := driveID(r)
	if err != nil {
		return err
	}
	d, err := h.svc.Register(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, d)
	return nil
}

func (h *Handler) unregister(w http.ResponseWriter, r *http.Request) error {
	id, err := driveID(r)
	if err != nil {
		return err
	}
	if err := h.svc.Unregister(r.Context(), h.uid(r), id); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) error {
	status := r.URL.Query().Get("status")
	if status == "All" {
		status = ""
	}
	ds, err := h.svc.ListAll(r.Context(), status)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": ds})
	return nil
}

func (h *Handler) adminGet(w http.ResponseWriter, r *http.Request) error {
	id, err := driveID(r)
	if err != nil {
		return err
	}
	d, err := h.svc.get(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, d)
	return nil
}

func (h *Handler) adminCreate(w http.ResponseWriter, r *http.Request) error {
	var in Input
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	d, err := h.svc.Create(r.Context(), in, h.uid(r), h.ip(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, d)
	return nil
}

func (h *Handler) adminUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := driveID(r)
	if err != nil {
		return err
	}
	var in Input
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	d, err := h.svc.Update(r.Context(), id, in, h.uid(r), h.ip(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, d)
	return nil
}

func (h *Handler) adminDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := driveID(r)
	if err != nil {
		return err
	}
	if err := h.svc.Delete(r.Context(), id, h.uid(r), h.ip(r)); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h *Handler) adminRegistrations(w http.ResponseWriter, r *http.Request) error {
	id, err := driveID(r)
	if err != nil {
		return err
	}
	rs, err := h.svc.Registrations(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": rs})
	return nil
}
