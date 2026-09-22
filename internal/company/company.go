// Package company covers recruiter accounts and the placement cell's approval of them.
package company

import (
	"context"
	"net/http"
	"strings"
	"time"

	"placementhub/internal/audit"
	"placementhub/internal/auth"
	"placementhub/internal/config"
	"placementhub/internal/db"
	"placementhub/internal/domain"
	"placementhub/internal/httpx"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Company matches the frontend's CompanyRecord.
type Company struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Color        string    `json:"color"`
	Industry     string    `json:"industry"`
	HR           string    `json:"hr"`
	Email        string    `json:"email"`
	Phone        string    `json:"phone"`
	Status       string    `json:"status"`
	StatusReason string    `json:"statusReason"`
	OpenRoles    int       `json:"openRoles"`
	Hires        int       `json:"hires"`
	LastVisit    *string   `json:"lastVisit"` // ISO date of the most recent drive, or null
}

const companySelect = `
SELECT c.id, c.name, c.color, c.industry, c.hr_name, c.email, c.phone, c.status, c.status_reason,
       (SELECT count(*) FROM jobs j WHERE j.company_id = c.id AND j.status = 'Open')::int,
       (SELECT count(*) FROM offers o WHERE o.company_id = c.id AND o.status = 'Accepted')::int,
       (SELECT to_char(max(d.starts_at AT TIME ZONE $1), 'YYYY-MM-DD') FROM drives d
         WHERE d.company_id = c.id AND d.starts_at <= now())
  FROM companies c`

func scan(row pgx.Row) (*Company, error) {
	var c Company
	err := row.Scan(&c.ID, &c.Name, &c.Color, &c.Industry, &c.HR, &c.Email, &c.Phone, &c.Status, &c.StatusReason,
		&c.OpenRoles, &c.Hires, &c.LastVisit)
	return &c, err
}

// ByUser loads the signed-in recruiter's company.
func (s *Service) ByUser(ctx context.Context, tz *time.Location, userID uuid.UUID) (*Company, error) {
	c, err := scan(s.pool.QueryRow(ctx, companySelect+` WHERE c.user_id = $2`, tz.String(), userID))
	if db.IsNoRows(err) {
		return nil, httpx.NotFound("company")
	}
	return c, err
}

type Update struct {
	Industry *string `json:"industry"`
	HR       *string `json:"hr"`
	Phone    *string `json:"phone"`
}

// UpdateOwn edits the fields a recruiter controls. Name and approval status stay with the placement cell.
func (s *Service) UpdateOwn(ctx context.Context, tz *time.Location, userID uuid.UUID, in Update) (*Company, error) {
	var v httpx.V
	for field, p := range map[string]*string{"industry": in.Industry, "hr": in.HR, "phone": in.Phone} {
		if p != nil {
			v.Text(field, *p, 0, 120)
		}
	}
	if in.HR != nil {
		v.Text("hr", *in.HR, 2, 120)
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE companies SET industry = COALESCE($2, industry), hr_name = COALESCE($3, hr_name), phone = COALESCE($4, phone)
		  WHERE user_id = $1`, userID, trim(in.Industry), trim(in.HR), trim(in.Phone))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("company")
	}
	return s.ByUser(ctx, tz, userID)
}

func trim(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

// List returns companies for the placement cell, optionally filtered by status.
func (s *Service) List(ctx context.Context, tz *time.Location, status string) ([]Company, error) {
	rows, err := s.pool.Query(ctx,
		companySelect+` WHERE ($2 = '' OR c.status = $2) ORDER BY c.name`, tz.String(), status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Company{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// SetStatus approves, rejects or resets a company. Rejecting also pulls its
// live work back: open jobs close, and jobs waiting for approval return to draft.
func (s *Service) SetStatus(ctx context.Context, tz *time.Location, id uuid.UUID, status, reason string, actor uuid.UUID, ip string) (*Company, error) {
	var v httpx.V
	v.OneOf("status", status, domain.CompanyApproved, domain.CompanyRejected, domain.CompanyPending)
	v.Text("reason", reason, 0, 500)
	if err := v.Err(); err != nil {
		return nil, err
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE companies SET status = $2, status_reason = $3, decided_at = now() WHERE id = $1`,
			id, status, strings.TrimSpace(reason))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.NotFound("company")
		}
		if status == domain.CompanyRejected {
			if _, err := tx.Exec(ctx,
				`UPDATE jobs SET status = 'Closed', closed_at = now(), updated_at = now() WHERE company_id = $1 AND status = 'Open'`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE jobs SET status = 'Draft', updated_at = now() WHERE company_id = $1 AND status = 'Pending'`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	audit.Log(ctx, s.pool, &actor, "company.status", "company", id.String(), ip, map[string]any{"status": status})
	cs, err := s.List(ctx, tz, "")
	if err != nil {
		return nil, err
	}
	for i := range cs {
		if cs[i].ID == id {
			return &cs[i], nil
		}
	}
	return nil, httpx.NotFound("company")
}

// ---- HTTP -----------------------------------------------------------------

type Handler struct {
	svc *Service
	cfg config.Config
}

func NewHandler(svc *Service, cfg config.Config) *Handler { return &Handler{svc: svc, cfg: cfg} }

// CompanyRoutes are mounted at /company behind RequireRole(company).
func (h *Handler) CompanyRoutes(r chi.Router) {
	r.Get("/profile", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		c, err := h.svc.ByUser(r.Context(), h.cfg.Location, auth.MustPrincipal(r).UserID)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, c)
		return nil
	}))
	r.Put("/profile", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		var in Update
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		c, err := h.svc.UpdateOwn(r.Context(), h.cfg.Location, auth.MustPrincipal(r).UserID, in)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, c)
		return nil
	}))
}

// AdminRoutes are mounted at /admin/companies behind RequireRole(admin).
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Get("/", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		status := r.URL.Query().Get("status")
		if status == "All" {
			status = ""
		}
		cs, err := h.svc.List(r.Context(), h.cfg.Location, status)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"items": cs})
		return nil
	}))
	r.Post("/{id}/reset-password", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			return httpx.NotFound("company")
		}
		pw, err := h.svc.ResetPassword(r.Context(), id, auth.MustPrincipal(r).UserID, httpx.ClientIP(r, h.cfg.TrustedProxy))
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"tempPassword": pw})
		return nil
	}))
	r.Patch("/{id}/status", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			return httpx.NotFound("company")
		}
		var in struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		c, err := h.svc.SetStatus(r.Context(), h.cfg.Location, id, in.Status, in.Reason,
			auth.MustPrincipal(r).UserID, httpx.ClientIP(r, h.cfg.TrustedProxy))
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, c)
		return nil
	}))
}

// IDByUser resolves the company row of a recruiter user (used by job handlers).
func IDByUser(ctx context.Context, q db.DBTX, userID uuid.UUID) (id uuid.UUID, status string, err error) {
	err = q.QueryRow(ctx, `SELECT id, status FROM companies WHERE user_id = $1`, userID).Scan(&id, &status)
	if db.IsNoRows(err) {
		return uuid.Nil, "", httpx.NotFound("company")
	}
	return id, status, err
}

// ResetPassword issues a new temporary password for a recruiter (there is no
// email channel), signing them out everywhere. It is shown once to the placement cell.
func (s *Service) ResetPassword(ctx context.Context, id, actor uuid.UUID, ip string) (string, error) {
	pw, err := auth.TempPassword()
	if err != nil {
		return "", err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return "", err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var uid uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT user_id FROM companies WHERE id = $1`, id).Scan(&uid); err != nil {
			if db.IsNoRows(err) {
				return httpx.NotFound("company")
			}
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE users SET password_hash = $2, must_change_password = true, updated_at = now() WHERE id = $1`, uid, hash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, uid)
		return err
	})
	if err != nil {
		return "", err
	}
	audit.Log(ctx, s.pool, &actor, "company.password_reset", "company", id.String(), ip, nil)
	return pw, nil
}
