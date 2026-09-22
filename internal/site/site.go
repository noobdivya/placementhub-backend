// Package site serves the public home page's configurable content and the
// list of recruiters.
package site

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"placementhub/internal/audit"
	"placementhub/internal/auth"
	"placementhub/internal/config"
	"placementhub/internal/db"
	"placementhub/internal/httpx"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const configKey = "config"
const maxConfigBytes = 64 << 10

type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Config returns the stored home-page document (college details, hero
// slides, desk messages, testimonials...), or {} if none is set yet.
func (s *Service) Config(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT value FROM site_settings WHERE key = $1`, configKey).Scan(&raw)
	if db.IsNoRows(err) {
		return json.RawMessage(`{}`), nil
	}
	return raw, err
}

func (s *Service) SetConfig(ctx context.Context, raw json.RawMessage) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO site_settings (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, configKey, []byte(raw))
	return err
}

type Recruiter struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Recruiters lists approved companies, most hires first.
func (s *Service) Recruiters(ctx context.Context) ([]Recruiter, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.name, c.color FROM companies c WHERE c.status = 'Approved'
		  ORDER BY (SELECT count(*) FROM offers o WHERE o.company_id = c.id AND o.status = 'Accepted') DESC, c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Recruiter{}
	for rows.Next() {
		var r Recruiter
		if err := rows.Scan(&r.Name, &r.Color); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Handler struct {
	svc *Service
	cfg config.Config
}

func NewHandler(svc *Service, cfg config.Config) *Handler { return &Handler{svc: svc, cfg: cfg} }

func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/site-config", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		raw, err := h.svc.Config(r.Context())
		if err != nil {
			return err
		}
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(raw)
		return nil
	}))
	r.Get("/recruiters", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		rs, err := h.svc.Recruiters(r.Context())
		if err != nil {
			return err
		}
		w.Header().Set("Cache-Control", "public, max-age=60")
		httpx.JSON(w, http.StatusOK, map[string]any{"items": rs})
		return nil
	}))
}

// AdminRoutes are mounted at /admin behind RequireRole(admin).
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Put("/site-config", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigBytes))
		if err != nil {
			return httpx.NewError(http.StatusRequestEntityTooLarge, "too_large", "site config is too large")
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
			return httpx.BadRequest("site config must be a JSON object")
		}
		if err := h.svc.SetConfig(r.Context(), body); err != nil {
			return err
		}
		actor := auth.MustPrincipal(r).UserID
		audit.Log(r.Context(), h.svc.pool, &actor, "site.config_updated", "site", "", httpx.ClientIP(r, h.cfg.TrustedProxy), nil)
		httpx.NoContent(w)
		return nil
	}))
}
