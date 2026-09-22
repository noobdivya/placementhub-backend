package job

import (
	"net/http"

	"placementhub/internal/auth"
	"placementhub/internal/config"
	"placementhub/internal/domain"
	"placementhub/internal/httpx"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Handler struct {
	svc *Service
	cfg config.Config
}

func NewHandler(svc *Service, cfg config.Config) *Handler { return &Handler{svc: svc, cfg: cfg} }

// StudentRoutes are mounted at /jobs behind RequireRole(student).
func (h *Handler) StudentRoutes(r chi.Router) {
	r.Get("/", httpx.Handle(h.studentList))
	r.Get("/{id}", httpx.Handle(h.studentGet))
}

// CompanyRoutes are mounted at /company/jobs behind RequireRole(company).
func (h *Handler) CompanyRoutes(r chi.Router) {
	r.Get("/", httpx.Handle(h.companyList))
	r.Post("/", httpx.Handle(h.companyCreate))
	r.Get("/{id}", httpx.Handle(h.companyGet))
	r.Put("/{id}", httpx.Handle(h.companyUpdate))
	r.Delete("/{id}", httpx.Handle(h.companyDelete))
	r.Post("/{id}/submit", httpx.Handle(h.companySubmit))
	r.Post("/{id}/close", httpx.Handle(h.companyClose))
}

// AdminRoutes are mounted at /admin/jobs behind RequireRole(admin).
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Get("/", httpx.Handle(h.adminList))
	r.Post("/{id}/approve", httpx.Handle(h.adminApprove))
	r.Post("/{id}/reject", httpx.Handle(h.adminReject))
	r.Post("/{id}/close", httpx.Handle(h.adminClose))
}

func (h *Handler) uid(r *http.Request) uuid.UUID { return auth.MustPrincipal(r).UserID }
func (h *Handler) ip(r *http.Request) string     { return httpx.ClientIP(r, h.cfg.TrustedProxy) }

func jobID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, httpx.NotFound("job")
	}
	return id, nil
}

func (h *Handler) studentList(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	page, limit, offset := httpx.Paging(r, 20, 100)
	f := ListFilter{Query: q.Get("q"), Sort: q.Get("sort"), EligibleOnly: q.Get("eligible") == "true", Limit: limit, Offset: offset}
	if t := q.Get("type"); t != "" && t != "All" {
		if t != domain.JobTypes[0] && t != domain.JobTypes[1] {
			return httpx.Unprocessable("bad_filter", "type must be Full-time or Internship")
		}
		f.Type = t
	}
	if f.Sort != "" && f.Sort != "deadline" && f.Sort != "ctc" {
		return httpx.Unprocessable("bad_filter", "sort must be deadline or ctc")
	}
	items, total, err := h.svc.ListForStudent(r.Context(), h.uid(r), f)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.Page[StudentJob]{Items: items, Total: total, Page: page, Limit: limit})
	return nil
}

func (h *Handler) studentGet(w http.ResponseWriter, r *http.Request) error {
	id, err := jobID(r)
	if err != nil {
		return err
	}
	j, err := h.svc.GetForStudent(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, j)
	return nil
}

func (h *Handler) companyList(w http.ResponseWriter, r *http.Request) error {
	jobs, err := h.svc.ListForCompany(r.Context(), h.uid(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": jobs})
	return nil
}

func (h *Handler) companyGet(w http.ResponseWriter, r *http.Request) error {
	id, err := jobID(r)
	if err != nil {
		return err
	}
	j, err := h.svc.GetOwned(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, j)
	return nil
}

// companyCreate saves a draft; with {"submit": true} it also sends it for approval.
func (h *Handler) companyCreate(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Input
		Submit bool `json:"submit"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	j, err := h.svc.Create(r.Context(), h.uid(r), in.Input)
	if err != nil {
		return err
	}
	if in.Submit {
		if j, err = h.svc.Submit(r.Context(), h.uid(r), j.ID); err != nil {
			return err
		}
	}
	httpx.JSON(w, http.StatusCreated, j)
	return nil
}

func (h *Handler) companyUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := jobID(r)
	if err != nil {
		return err
	}
	var in Input
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	j, err := h.svc.Update(r.Context(), h.uid(r), id, in)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, j)
	return nil
}

func (h *Handler) companyDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := jobID(r)
	if err != nil {
		return err
	}
	if err := h.svc.Delete(r.Context(), h.uid(r), id); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h *Handler) companySubmit(w http.ResponseWriter, r *http.Request) error {
	id, err := jobID(r)
	if err != nil {
		return err
	}
	j, err := h.svc.Submit(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, j)
	return nil
}

func (h *Handler) companyClose(w http.ResponseWriter, r *http.Request) error {
	id, err := jobID(r)
	if err != nil {
		return err
	}
	j, err := h.svc.Close(r.Context(), h.uid(r), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, j)
	return nil
}

func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) error {
	status := r.URL.Query().Get("status")
	if status == "All" {
		status = ""
	}
	jobs, err := h.svc.ListForAdmin(r.Context(), status)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": jobs})
	return nil
}

func (h *Handler) adminApprove(w http.ResponseWriter, r *http.Request) error {
	id, err := jobID(r)
	if err != nil {
		return err
	}
	j, n, err := h.svc.Approve(r.Context(), id, h.uid(r), h.ip(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"job": j, "studentsNotified": n})
	return nil
}

func (h *Handler) adminReject(w http.ResponseWriter, r *http.Request) error {
	id, err := jobID(r)
	if err != nil {
		return err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	j, err := h.svc.Reject(r.Context(), id, in.Reason, h.uid(r), h.ip(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, j)
	return nil
}

func (h *Handler) adminClose(w http.ResponseWriter, r *http.Request) error {
	id, err := jobID(r)
	if err != nil {
		return err
	}
	j, err := h.svc.AdminClose(r.Context(), id, h.uid(r), h.ip(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, j)
	return nil
}
