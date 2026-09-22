package student

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

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

// StudentRoutes are mounted at /me behind Authenticate + RequireRole(student).
func (h *Handler) StudentRoutes(r chi.Router) {
	r.Get("/profile", httpx.Handle(h.getProfile))
	r.Put("/profile", httpx.Handle(h.putProfile))
	r.Post("/skills", httpx.Handle(h.addSkill))
	r.Delete("/skills", httpx.Handle(h.removeSkill))
	r.Post("/resume", httpx.Handle(h.uploadResume))
	r.Get("/resume", httpx.Handle(h.downloadResume))
}

// AdminRoutes are mounted at /admin/students behind Authenticate + RequireRole(admin).
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Get("/", httpx.Handle(h.adminList))
	r.Post("/", httpx.Handle(h.adminCreate))
	r.Post("/import", httpx.Handle(h.adminImport))
	r.Get("/export.csv", httpx.Handle(h.adminExport))
	r.Get("/{id}", httpx.Handle(h.adminGet))
	r.Patch("/{id}", httpx.Handle(h.adminUpdate))
	r.Post("/{id}/activate", httpx.Handle(h.adminSetActive(true)))
	r.Post("/{id}/deactivate", httpx.Handle(h.adminSetActive(false)))
	r.Post("/{id}/reset-password", httpx.Handle(h.adminResetPassword))
}

func (h *Handler) ip(r *http.Request) string { return httpx.ClientIP(r, h.cfg.TrustedProxy) }

func (h *Handler) getProfile(w http.ResponseWriter, r *http.Request) error {
	p, err := h.svc.ByUser(r.Context(), auth.MustPrincipal(r).UserID)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, p)
	return nil
}

func (h *Handler) putProfile(w http.ResponseWriter, r *http.Request) error {
	var in ProfileUpdate
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	p, err := h.svc.UpdateProfile(r.Context(), auth.MustPrincipal(r).UserID, in)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, p)
	return nil
}

func (h *Handler) addSkill(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Skill string `json:"skill"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	skills, err := h.svc.AddSkill(r.Context(), auth.MustPrincipal(r).UserID, in.Skill)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"skills": skills})
	return nil
}

// removeSkill takes ?name= because skills such as "C/C++" do not fit in a path segment.
func (h *Handler) removeSkill(w http.ResponseWriter, r *http.Request) error {
	name := r.URL.Query().Get("name")
	if name == "" {
		return httpx.BadRequest("name is required")
	}
	skills, err := h.svc.RemoveSkill(r.Context(), auth.MustPrincipal(r).UserID, name)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"skills": skills})
	return nil
}

func (h *Handler) uploadResume(w http.ResponseWriter, r *http.Request) error {
	// Cap the whole request a little above the file limit to cover multipart framing.
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxUploadBytes()+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return httpx.NewError(http.StatusRequestEntityTooLarge, "too_large", "upload is too large or malformed")
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return httpx.BadRequest(`attach the resume as multipart field "file"`)
	}
	defer f.Close()
	meta, err := h.svc.SaveResume(r.Context(), auth.MustPrincipal(r).UserID, hdr.Filename, f)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, meta)
	return nil
}

func (h *Handler) downloadResume(w http.ResponseWriter, r *http.Request) error {
	sid, err := h.svc.StudentID(r.Context(), auth.MustPrincipal(r).UserID)
	if err != nil {
		return err
	}
	return ServeResume(w, r, h.svc, sid)
}

// ServeResume streams a resume as a download. Shared with the company and admin views.
func ServeResume(w http.ResponseWriter, r *http.Request, svc *Service, studentID uuid.UUID) error {
	res, err := svc.OpenResume(r.Context(), studentID)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", strconv.FormatInt(res.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": res.Filename}))
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = io.Copy(w, res.Body)
	return nil
}

// ---- admin ----------------------------------------------------------------

func studentID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, httpx.NotFound("student")
	}
	return id, nil
}

func filterFrom(r *http.Request, limit, offset int) ListFilter {
	q := r.URL.Query()
	status := q.Get("status")
	if status == "All" {
		status = ""
	}
	branch := q.Get("branch")
	if branch == "All" {
		branch = ""
	}
	return ListFilter{Query: q.Get("q"), Branch: branch, Status: status, Limit: limit, Offset: offset}
}

func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) error {
	page, limit, offset := httpx.Paging(r, 25, 200)
	f := filterFrom(r, limit, offset)
	if f.Status != "" && f.Status != domain.StudentPlaced && f.Status != domain.StudentInProcess && f.Status != domain.StudentUnplaced {
		return httpx.Unprocessable("bad_filter", "status must be Placed, In process or Unplaced")
	}
	items, total, err := h.svc.List(r.Context(), f)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.Page[ListItem]{Items: items, Total: total, Page: page, Limit: limit})
	return nil
}

func (h *Handler) adminExport(w http.ResponseWriter, r *http.Request) error {
	items, _, err := h.svc.List(r.Context(), filterFrom(r, 100000, 0))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, s := range items {
		company, ctc := "", ""
		if s.Company != nil {
			company = *s.Company
		}
		if s.CTC != nil {
			ctc = strconv.FormatFloat(*s.CTC, 'f', -1, 64)
		}
		rows = append(rows, []string{s.Roll, s.Name, s.Email, s.Branch, fmt.Sprintf("%.2f", s.CGPA), s.Status, company, ctc})
	}
	return httpx.WriteCSV(w, "students.csv",
		[]string{"roll", "name", "email", "branch", "cgpa", "status", "company", "ctc_lpa"}, rows)
}

func (h *Handler) adminGet(w http.ResponseWriter, r *http.Request) error {
	id, err := studentID(r)
	if err != nil {
		return err
	}
	p, err := h.svc.ByID(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, p)
	return nil
}

func (h *Handler) adminCreate(w http.ResponseWriter, r *http.Request) error {
	var in NewStudent
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	c, err := h.svc.Create(r.Context(), in, auth.MustPrincipal(r).UserID, h.ip(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, c)
	return nil
}

func (h *Handler) adminImport(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 3<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return httpx.NewError(http.StatusRequestEntityTooLarge, "too_large", "upload is too large or malformed")
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		return httpx.BadRequest(`attach the CSV as multipart field "file"`)
	}
	defer f.Close()
	res, err := h.svc.Import(r.Context(), f, auth.MustPrincipal(r).UserID, h.ip(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, res)
	return nil
}

func (h *Handler) adminUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := studentID(r)
	if err != nil {
		return err
	}
	var in AdminUpdate
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	p, err := h.svc.AdminUpdate(r.Context(), id, in, auth.MustPrincipal(r).UserID, h.ip(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, p)
	return nil
}

func (h *Handler) adminSetActive(active bool) httpx.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := studentID(r)
		if err != nil {
			return err
		}
		if err := h.svc.SetActive(r.Context(), id, active, auth.MustPrincipal(r).UserID, h.ip(r)); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}
}

func (h *Handler) adminResetPassword(w http.ResponseWriter, r *http.Request) error {
	id, err := studentID(r)
	if err != nil {
		return err
	}
	pw, err := h.svc.ResetPassword(r.Context(), id, auth.MustPrincipal(r).UserID, h.ip(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"tempPassword": pw})
	return nil
}
