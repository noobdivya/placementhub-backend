// Package app wires configuration, services and HTTP routes together.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"placementhub/internal/application"
	"placementhub/internal/auth"
	"placementhub/internal/company"
	"placementhub/internal/config"
	"placementhub/internal/domain"
	"placementhub/internal/drive"
	"placementhub/internal/email"
	"placementhub/internal/httpx"
	"placementhub/internal/job"
	"placementhub/internal/notice"
	"placementhub/internal/notify"
	"placementhub/internal/push"
	"placementhub/internal/report"
	"placementhub/internal/round"
	"placementhub/internal/scheduler"
	"placementhub/internal/site"
	"placementhub/internal/storage"
	"placementhub/internal/student"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options lets tests swap the clock, the push/email transports and file storage.
type Options struct {
	Now         func() time.Time
	Sender      push.Sender
	EmailSender email.Sender
	Storage     storage.Storage
}

type App struct {
	Handler   http.Handler
	Cfg       config.Config
	Pool      *pgxpool.Pool
	Auth      *auth.Service
	Jobs      *job.Service
	Apps      *application.Service
	Drives    *drive.Service
	Students  *student.Service
	Reports   *report.Service
	Push      *push.Worker
	Email     *email.Worker
	Scheduler *scheduler.Scheduler
}

func New(cfg config.Config, pool *pgxpool.Pool, opts Options) (*App, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	store := opts.Storage
	if store == nil {
		l, err := storage.NewLocal(cfg.UploadDir)
		if err != nil {
			return nil, err
		}
		store = l
	}
	sender := opts.Sender
	if sender == nil && cfg.PushEnabled() {
		sender = push.NewWebPush(cfg.VAPIDPublic, cfg.VAPIDPrivate, cfg.VAPIDSubject)
	}
	emailSender := opts.EmailSender
	if emailSender == nil && cfg.EmailEnabled() {
		emailSender = email.NewResend(cfg.ResendAPIKey, cfg.EmailFrom)
	}

	notifier := notify.New(cfg.PushEnabled() || opts.Sender != nil, cfg.EmailEnabled() || opts.EmailSender != nil, cfg.FrontendURL)
	authSvc := auth.NewService(pool, cfg)
	authSvc.Now = now
	studentSvc := student.NewService(pool, store, cfg.MaxUploadBytes())
	companySvc := company.NewService(pool)
	jobSvc := job.NewService(pool, notifier, cfg)
	jobSvc.Now = now
	appSvc := application.NewService(pool, notifier, cfg)
	appSvc.Now = now
	driveSvc := drive.NewService(pool, notifier, cfg)
	driveSvc.Now = now
	roundSvc := round.NewService(pool, notifier, cfg)
	roundSvc.Now = now
	noticeSvc := notice.NewService(pool, notifier, cfg)
	siteSvc := site.NewService(pool)
	reportSvc := report.NewService(pool, cfg)
	reportSvc.Now = now
	notifySvc := notify.NewService(pool, cfg.PushEnabled() || opts.Sender != nil, cfg.EmailEnabled() || opts.EmailSender != nil)

	a := &App{Cfg: cfg, Pool: pool, Auth: authSvc, Jobs: jobSvc, Apps: appSvc, Drives: driveSvc,
		Students: studentSvc, Reports: reportSvc}
	if sender != nil {
		a.Push = push.NewWorker(pool, sender)
		a.Push.Now = now
	}
	if emailSender != nil {
		a.Email = email.NewWorker(pool, emailSender)
		a.Email.Now = now
	}
	a.Scheduler = scheduler.New(pool, jobSvc, appSvc, driveSvc, roundSvc)
	a.Scheduler.Now = now

	authH := auth.NewHandler(authSvc, cfg)
	studentH := student.NewHandler(studentSvc, cfg)
	companyH := company.NewHandler(companySvc, cfg)
	jobH := job.NewHandler(jobSvc, cfg)
	appH := application.NewHandler(appSvc, studentSvc, cfg)
	driveH := drive.NewHandler(driveSvc, cfg)
	roundH := round.NewHandler(roundSvc, cfg)
	noticeH := notice.NewHandler(noticeSvc, cfg)
	siteH := site.NewHandler(siteSvc, cfg)
	reportH := report.NewHandler(reportSvc)
	notifyH := notify.NewHandler(notifySvc, cfg)

	r := chi.NewRouter()
	r.Use(httpx.WithRequestID, httpx.AccessLog, httpx.Recover,
		httpx.SecurityHeaders(cfg.IsProd()), httpx.CORS(cfg.CORSOrigins),
		httpx.NewLimiter(600, 120).Limit(cfg.TrustedProxy))
	r.NotFound(httpx.Handle(func(http.ResponseWriter, *http.Request) error { return httpx.NotFound("route") }))
	r.MethodNotAllowed(httpx.Handle(func(http.ResponseWriter, *http.Request) error {
		return httpx.NewError(http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			return httpx.NewError(http.StatusServiceUnavailable, "not_ready", "database unavailable")
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return nil
	}))

	// Public.
	r.Route("/auth", authH.Public)
	notifyH.PublicRoutes(r)
	noticeH.PublicRoutes(r)
	siteH.PublicRoutes(r)

	// Authenticated.
	r.Group(func(r chi.Router) {
		r.Use(authH.Auth)

		r.Route("/me", func(r chi.Router) {
			r.Get("/", httpx.Handle(a.me(companySvc, cfg)))
			r.Group(func(r chi.Router) { // student-only
				r.Use(auth.RequireRole(domain.RoleStudent))
				studentH.StudentRoutes(r)
				appH.MeRoutes(r)
				roundH.MeRoutes(r)
				notifyH.StudentRoutes(r)
			})
		})
		r.Route("/jobs", func(r chi.Router) {
			r.Use(auth.RequireRole(domain.RoleStudent))
			jobH.StudentRoutes(r)
			appH.JobRoutes(r)
			roundH.StudentJobRoutes(r)
		})
		r.With(auth.RequireRole(domain.RoleStudent)).Route("/drives", driveH.StudentRoutes)

		r.Route("/company", func(r chi.Router) {
			r.Use(auth.RequireRole(domain.RoleCompany))
			companyH.CompanyRoutes(r)
			r.Route("/jobs", func(r chi.Router) {
				jobH.CompanyRoutes(r)
				roundH.CompanyJobRoutes(r)
			})
			appH.CompanyRoutes(r)
			roundH.CompanyRoutes(r)
		})

		r.Route("/admin", func(r chi.Router) {
			r.Use(auth.RequireRole(domain.RoleAdmin))
			r.Route("/students", func(r chi.Router) {
				studentH.AdminRoutes(r)
				r.Get("/{id}/applications", appH.HistoryHandler())
			})
			appH.AdminRoutes(r)
			r.Post("/admins", authH.AdminCreateAdmin())
			r.Route("/companies", companyH.AdminRoutes)
			r.Route("/jobs", jobH.AdminRoutes)
			r.Route("/drives", driveH.AdminRoutes)
			r.Route("/notices", noticeH.AdminRoutes)
			r.Route("/reports", reportH.AdminRoutes)
			siteH.AdminRoutes(r)
		})
	})

	a.Handler = r
	return a, nil
}

// me returns the signed-in account with its role-specific profile.
func (a *App) me(companies *company.Service, cfg config.Config) httpx.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p := auth.MustPrincipal(r)
		u, err := a.Auth.GetUser(r.Context(), p.UserID)
		if err != nil {
			return err
		}
		out := map[string]any{"user": u}
		switch u.Role {
		case domain.RoleStudent:
			prof, err := a.Students.ByUser(r.Context(), u.ID)
			if err != nil {
				return err
			}
			out["student"] = prof
		case domain.RoleCompany:
			c, err := companies.ByUser(r.Context(), cfg.Location, u.ID)
			if err != nil {
				return err
			}
			out["company"] = c
		}
		httpx.JSON(w, http.StatusOK, out)
		return nil
	}
}

// StartWorkers launches the push worker and scheduler; they stop when ctx ends.
func (a *App) StartWorkers(ctx context.Context) {
	if !a.Cfg.RunWorkers {
		slog.Info("background workers disabled (RUN_WORKERS=false)")
		return
	}
	if a.Push != nil {
		go a.Push.Run(ctx)
	} else {
		slog.Warn("web push is not configured (set VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY); browser notifications are disabled")
	}
	if a.Email != nil {
		go a.Email.Run(ctx)
	} else {
		slog.Warn("email is not configured (set RESEND_API_KEY / EMAIL_FROM); email notifications are disabled")
	}
	go a.Scheduler.Run(ctx)
}
