// Package report produces the placement cell's dashboard numbers.
//
// "Placed" always means holding an Accepted offer; offers that were rescinded
// never count.
package report

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"placementhub/internal/config"
	"placementhub/internal/domain"
	"placementhub/internal/httpx"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool *pgxpool.Pool
	loc  *time.Location
	Now  func() time.Time
}

func NewService(pool *pgxpool.Pool, cfg config.Config) *Service {
	return &Service{pool: pool, loc: cfg.Location, Now: time.Now}
}

type Summary struct {
	TotalStudents    int     `json:"totalStudents"`
	Placed           int     `json:"placed"`
	AvgCTC           float64 `json:"avgCtc"`
	HighestCTC       float64 `json:"highestCtc"`
	CompaniesVisited int     `json:"companiesVisited"`
	Offers           int     `json:"offers"`
}

func (s *Service) Summary(ctx context.Context) (*Summary, error) {
	var out Summary
	err := s.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM students)::int,
       (SELECT count(DISTINCT student_id) FROM offers WHERE status = 'Accepted')::int,
       COALESCE((SELECT round(avg(ctc), 1) FROM offers WHERE status = 'Accepted'), 0)::float8,
       COALESCE((SELECT max(ctc) FROM offers WHERE status = 'Accepted'), 0)::float8,
       (SELECT count(*) FROM (
            SELECT company_id FROM drives WHERE starts_at <= $1
            UNION SELECT company_id FROM offers WHERE status <> 'Rescinded') v)::int,
       (SELECT count(*) FROM offers WHERE status <> 'Rescinded')::int`, s.Now()).
		Scan(&out.TotalStudents, &out.Placed, &out.AvgCTC, &out.HighestCTC, &out.CompaniesVisited, &out.Offers)
	return &out, err
}

type Point struct {
	Label string `json:"label"`
	Value int    `json:"value"`
}

// MonthlyOffers counts offers made in each of the last six months (zero-filled).
func (s *Service) MonthlyOffers(ctx context.Context) ([]Point, error) {
	rows, err := s.pool.Query(ctx, `
SELECT to_char(m, 'Mon'), count(o.id)::int
  FROM generate_series(date_trunc('month', $1::timestamptz AT TIME ZONE $2) - interval '5 months',
                       date_trunc('month', $1::timestamptz AT TIME ZONE $2), interval '1 month') m
  LEFT JOIN offers o ON o.status <> 'Rescinded' AND date_trunc('month', o.created_at AT TIME ZONE $2) = m
 GROUP BY m ORDER BY m`, s.Now(), s.loc.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Point{}
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.Label, &p.Value); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type BranchStat struct {
	Branch string `json:"branch"`
	Total  int    `json:"total"`
	Placed int    `json:"placed"`
}

func (s *Service) Branches(ctx context.Context) ([]BranchStat, error) {
	rows, err := s.pool.Query(ctx, `
SELECT b.branch, count(s.id)::int,
       count(s.id) FILTER (WHERE EXISTS (SELECT 1 FROM offers o WHERE o.student_id = s.id AND o.status = 'Accepted'))::int
  FROM unnest($1::text[]) WITH ORDINALITY AS b(branch, n)
  LEFT JOIN students s ON s.branch = b.branch
 GROUP BY b.branch, b.n ORDER BY b.n`, domain.Branches)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BranchStat{}
	for rows.Next() {
		var b BranchStat
		if err := rows.Scan(&b.Branch, &b.Total, &b.Placed); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CTCBands buckets accepted offers: <6, 6-10, 10-15, 15-25, 25+ LPA.
func (s *Service) CTCBands(ctx context.Context) ([]Point, error) {
	rows, err := s.pool.Query(ctx, `
SELECT b.label, count(o.id)::int
  FROM (VALUES (1, '< 6 LPA', 0::numeric, 6::numeric), (2, '6–10', 6, 10), (3, '10–15', 10, 15),
               (4, '15–25', 15, 25), (5, '25+', 25, 1000000)) AS b(n, label, lo, hi)
  LEFT JOIN offers o ON o.status = 'Accepted' AND o.ctc >= b.lo AND o.ctc < b.hi
 GROUP BY b.n, b.label ORDER BY b.n`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Point{}
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.Label, &p.Value); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type Recruiter struct {
	Name  string `json:"name"`
	Hires int    `json:"hires"`
	Color string `json:"color"`
}

func (s *Service) TopRecruiters(ctx context.Context, limit int) ([]Recruiter, error) {
	rows, err := s.pool.Query(ctx, `
SELECT c.name, count(o.id)::int AS hires, c.color
  FROM companies c JOIN offers o ON o.company_id = c.id AND o.status = 'Accepted'
 GROUP BY c.id ORDER BY hires DESC, c.name LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Recruiter{}
	for rows.Next() {
		var r Recruiter
		if err := rows.Scan(&r.Name, &r.Hires, &r.Color); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type RecentOffer struct {
	Student string  `json:"student"`
	Company string  `json:"company"`
	Role    string  `json:"role"`
	CTC     float64 `json:"ctc"`
	Date    string  `json:"date"`
	Status  string  `json:"status"`
}

func (s *Service) RecentOffers(ctx context.Context, limit int) ([]RecentOffer, error) {
	rows, err := s.pool.Query(ctx, `
SELECT u.name, c.name, j.role, o.ctc::float8, to_char(o.created_at AT TIME ZONE $1, 'YYYY-MM-DD'), o.status
  FROM offers o JOIN students s ON s.id = o.student_id JOIN users u ON u.id = s.user_id
  JOIN companies c ON c.id = o.company_id JOIN jobs j ON j.id = o.job_id
 WHERE o.status <> 'Rescinded' ORDER BY o.created_at DESC, o.id LIMIT $2`, s.loc.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecentOffer{}
	for rows.Next() {
		var r RecentOffer
		if err := rows.Scan(&r.Student, &r.Company, &r.Role, &r.CTC, &r.Date, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PlacementsCSV lists every accepted offer.
func (s *Service) PlacementsCSV(ctx context.Context) ([][]string, error) {
	rows, err := s.pool.Query(ctx, `
SELECT s.roll, u.name, s.branch, c.name, j.role, o.ctc::float8, to_char(o.responded_at AT TIME ZONE $1, 'YYYY-MM-DD')
  FROM offers o JOIN students s ON s.id = o.student_id JOIN users u ON u.id = s.user_id
  JOIN companies c ON c.id = o.company_id JOIN jobs j ON j.id = o.job_id
 WHERE o.status = 'Accepted' ORDER BY s.roll`, s.loc.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := [][]string{}
	for rows.Next() {
		var roll, name, branch, company, role, date string
		var ctc float64
		if err := rows.Scan(&roll, &name, &branch, &company, &role, &ctc, &date); err != nil {
			return nil, err
		}
		out = append(out, []string{roll, name, branch, company, role, strconv.FormatFloat(ctc, 'f', -1, 64), date})
	}
	return out, rows.Err()
}

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// AdminRoutes are mounted at /admin/reports behind RequireRole(admin).
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Get("/summary", h.json(func(ctx context.Context) (any, error) { return h.svc.Summary(ctx) }))
	r.Get("/monthly-offers", h.json(func(ctx context.Context) (any, error) { return h.svc.MonthlyOffers(ctx) }))
	r.Get("/branches", h.json(func(ctx context.Context) (any, error) { return h.svc.Branches(ctx) }))
	r.Get("/ctc-bands", h.json(func(ctx context.Context) (any, error) { return h.svc.CTCBands(ctx) }))
	r.Get("/top-recruiters", h.json(func(ctx context.Context) (any, error) { return h.svc.TopRecruiters(ctx, 5) }))
	r.Get("/recent-offers", h.json(func(ctx context.Context) (any, error) { return h.svc.RecentOffers(ctx, 10) }))
	r.Get("/overview", httpx.Handle(h.overview))
	r.Get("/placements.csv", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		rows, err := h.svc.PlacementsCSV(r.Context())
		if err != nil {
			return err
		}
		return httpx.WriteCSV(w, "placements.csv",
			[]string{"roll", "name", "branch", "company", "role", "ctc_lpa", "accepted_on"}, rows)
	}))
}

func (h *Handler) json(fn func(context.Context) (any, error)) http.HandlerFunc {
	return httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		v, err := fn(r.Context())
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, v)
		return nil
	})
}

// overview returns every dashboard block in one call.
func (h *Handler) overview(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var out struct {
		Summary       *Summary      `json:"stats"`
		MonthlyOffers []Point       `json:"monthlyOffers"`
		Branches      []BranchStat  `json:"branchStats"`
		CTCBands      []Point       `json:"ctcBands"`
		TopRecruiters []Recruiter   `json:"topRecruiters"`
		RecentOffers  []RecentOffer `json:"recentOffers"`
	}
	var err error
	if out.Summary, err = h.svc.Summary(ctx); err != nil {
		return err
	}
	if out.MonthlyOffers, err = h.svc.MonthlyOffers(ctx); err != nil {
		return err
	}
	if out.Branches, err = h.svc.Branches(ctx); err != nil {
		return err
	}
	if out.CTCBands, err = h.svc.CTCBands(ctx); err != nil {
		return err
	}
	if out.TopRecruiters, err = h.svc.TopRecruiters(ctx, 5); err != nil {
		return err
	}
	if out.RecentOffers, err = h.svc.RecentOffers(ctx, 10); err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}
