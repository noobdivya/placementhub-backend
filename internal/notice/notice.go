// Package notice covers the public notice board and the placement cell's broadcasts.
package notice

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"placementhub/internal/audit"
	"placementhub/internal/auth"
	"placementhub/internal/config"
	"placementhub/internal/db"
	"placementhub/internal/domain"
	"placementhub/internal/httpx"
	"placementhub/internal/notify"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool     *pgxpool.Pool
	notifier *notify.Notifier
	loc      *time.Location
}

func NewService(pool *pgxpool.Pool, n *notify.Notifier, cfg config.Config) *Service {
	return &Service{pool: pool, notifier: n, loc: cfg.Location}
}

// Notice matches the frontend's notices entries.
type Notice struct {
	ID    uuid.UUID `json:"id"`
	Date  string    `json:"date"`
	Tag   string    `json:"tag"`
	Title string    `json:"title"`
}

func (s *Service) List(ctx context.Context, limit int) ([]Notice, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, to_char(published_at AT TIME ZONE $1, 'YYYY-MM-DD'), tag, title
		   FROM notices ORDER BY published_at DESC, id LIMIT $2`, s.loc.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notice{}
	for rows.Next() {
		var n Notice
		if err := rows.Scan(&n.ID, &n.Date, &n.Tag, &n.Title); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

type Input struct {
	Title    string   `json:"title"`
	Tag      string   `json:"tag"`
	Notify   bool     `json:"notify"`   // also push it to students
	Branches []string `json:"branches"` // with notify: limit to these branches; empty = every student
}

// Create publishes a notice and, if asked, broadcasts it to students.
func (s *Service) Create(ctx context.Context, in Input, actor uuid.UUID, ip string) (*Notice, int64, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Tag == "" {
		in.Tag = "General"
	}
	var v httpx.V
	v.Text("title", in.Title, 3, 200)
	v.OneOf("tag", in.Tag, domain.NoticeTags...)
	for _, b := range in.Branches {
		if !domain.ValidBranch(b) {
			v.Add("branches", "unknown branch: "+b)
			break
		}
	}
	if err := v.Err(); err != nil {
		return nil, 0, err
	}
	var id uuid.UUID
	var notified int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO notices (title, tag, created_by) VALUES ($1, $2, $3) RETURNING id`,
			in.Title, in.Tag, actor).Scan(&id); err != nil {
			return err
		}
		if !in.Notify {
			return nil
		}
		var err error
		notified, err = s.notifier.ToUsers(ctx, tx,
			`SELECT s.user_id FROM students s JOIN users u ON u.id = s.user_id
			  WHERE u.active AND (cardinality($1::text[]) = 0 OR s.branch = ANY($1::text[]))`,
			[]any{in.Branches}, notify.Spec{
				Type:      domain.NotifNotice,
				Title:     "Placement cell: " + in.Tag,
				Body:      in.Title,
				Link:      "/",
				Data:      map[string]any{"noticeId": id},
				DedupeKey: "notice:" + id.String(),
			})
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	audit.Log(ctx, s.pool, &actor, "notice.created", "notice", id.String(), ip, map[string]any{"notified": notified})
	list, err := s.List(ctx, 1000)
	if err != nil {
		return nil, 0, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], notified, nil
		}
	}
	return nil, 0, httpx.NotFound("notice")
}

func (s *Service) Delete(ctx context.Context, id, actor uuid.UUID, ip string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM notices WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("notice")
	}
	audit.Log(ctx, s.pool, &actor, "notice.deleted", "notice", id.String(), ip, nil)
	return nil
}

type Handler struct {
	svc *Service
	cfg config.Config
}

func NewHandler(svc *Service, cfg config.Config) *Handler { return &Handler{svc: svc, cfg: cfg} }

// PublicRoutes serves the notice board without authentication.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/notices", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit < 1 || limit > 100 {
			limit = 20
		}
		items, err := h.svc.List(r.Context(), limit)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
		return nil
	}))
}

// AdminRoutes are mounted at /admin/notices behind RequireRole(admin).
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Post("/", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		var in Input
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		n, notified, err := h.svc.Create(r.Context(), in, auth.MustPrincipal(r).UserID, httpx.ClientIP(r, h.cfg.TrustedProxy))
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, map[string]any{"notice": n, "studentsNotified": notified})
		return nil
	}))
	r.Delete("/{id}", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			return httpx.NotFound("notice")
		}
		if err := h.svc.Delete(r.Context(), id, auth.MustPrincipal(r).UserID, httpx.ClientIP(r, h.cfg.TrustedProxy)); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
}
