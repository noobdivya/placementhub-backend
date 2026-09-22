package auth

import (
	"net/http"
	"slices"
	"time"

	"placementhub/internal/config"
	"placementhub/internal/httpx"

	"github.com/go-chi/chi/v5"
)

const refreshCookie = "ph_refresh"

type Handler struct {
	svc      *Service
	cfg      config.Config
	ipLimit  *httpx.Limiter
	acctLim  *httpx.Limiter
	Auth     func(http.Handler) http.Handler
	authOnly func(http.Handler) http.Handler
}

func NewHandler(svc *Service, cfg config.Config) *Handler {
	return &Handler{
		svc:     svc,
		cfg:     cfg,
		ipLimit: httpx.NewLimiter(30, 10), // 30/min per IP, bursts of 10
		acctLim: httpx.NewLimiter(10, 5),  // 10/min per account, bursts of 5
		Auth:    Authenticate(cfg.JWTSecret, svc.Now),
	}
}

// Public mounts the unauthenticated endpoints under /auth.
func (h *Handler) Public(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.ipLimit.Limit(h.cfg.TrustedProxy))
		r.Post("/login", httpx.Handle(h.login))
		r.Post("/refresh", httpx.Handle(h.refresh))
		r.Post("/logout", httpx.Handle(h.logout))
		r.Post("/register/company", httpx.Handle(h.registerCompany))
	})
	r.With(h.Auth).Post("/change-password", httpx.Handle(h.changePassword))
}

type sessionResponse struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
	User        User      `json:"user"`
}

func (h *Handler) respond(w http.ResponseWriter, status int, s *Session) {
	h.setRefreshCookie(w, s.RefreshToken, s.RefreshExpires)
	httpx.JSON(w, status, sessionResponse{AccessToken: s.AccessToken, ExpiresAt: s.AccessExpires, User: s.User})
}

func (h *Handler) setRefreshCookie(w http.ResponseWriter, token string, exp time.Time) {
	c := &http.Cookie{
		Name: refreshCookie, Value: token, Path: "/auth", HttpOnly: true,
		Secure: h.cfg.CookieSecure, SameSite: h.cfg.CookieSameSite, Expires: exp,
	}
	if token == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// checkOrigin blocks cross-site requests that ride on the refresh cookie.
func (h *Handler) checkOrigin(r *http.Request) error {
	if o := r.Header.Get("Origin"); o != "" && !slices.Contains(h.cfg.CORSOrigins, o) {
		return httpx.Forbidden("origin not allowed")
	}
	return nil
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	email := normEmail(in.Email)
	if email == "" || in.Password == "" {
		return httpx.BadRequest("email and password are required")
	}
	if !h.acctLim.Allow(email) {
		return httpx.NewError(http.StatusTooManyRequests, "rate_limited", "too many attempts for this account, try again shortly")
	}
	sess, err := h.svc.Login(r.Context(), email, in.Password, r.UserAgent(), httpx.ClientIP(r, h.cfg.TrustedProxy))
	if err != nil {
		return err
	}
	h.respond(w, http.StatusOK, sess)
	return nil
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) error {
	if err := h.checkOrigin(r); err != nil {
		return err
	}
	c, _ := r.Cookie(refreshCookie)
	tok := ""
	if c != nil {
		tok = c.Value
	}
	sess, err := h.svc.Refresh(r.Context(), tok, r.UserAgent(), httpx.ClientIP(r, h.cfg.TrustedProxy))
	if err != nil {
		h.setRefreshCookie(w, "", time.Time{})
		return err
	}
	h.respond(w, http.StatusOK, sess)
	return nil
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) error {
	if err := h.checkOrigin(r); err != nil {
		return err
	}
	if c, err := r.Cookie(refreshCookie); err == nil {
		if err := h.svc.Logout(r.Context(), c.Value); err != nil {
			return err
		}
	}
	h.setRefreshCookie(w, "", time.Time{})
	httpx.NoContent(w)
	return nil
}

func (h *Handler) registerCompany(w http.ResponseWriter, r *http.Request) error {
	var in CompanyRegistration
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	u, err := h.svc.RegisterCompany(r.Context(), in, httpx.ClientIP(r, h.cfg.TrustedProxy))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"user":    u,
		"message": "Registered. Your company is pending approval by the placement cell; you can draft jobs meanwhile.",
	})
	return nil
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	p := MustPrincipal(r)
	sess, err := h.svc.ChangePassword(r.Context(), p.UserID, in.CurrentPassword, in.NewPassword,
		r.UserAgent(), httpx.ClientIP(r, h.cfg.TrustedProxy))
	if err != nil {
		return err
	}
	h.respond(w, http.StatusOK, sess)
	return nil
}

// AdminCreateAdmin serves POST /admin/admins.
func (h *Handler) AdminCreateAdmin() http.HandlerFunc {
	return httpx.Handle(func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		}
		if err := httpx.Decode(w, r, &in); err != nil {
			return err
		}
		u, pw, err := h.svc.CreateAdmin(r.Context(), in.Name, in.Email, MustPrincipal(r).UserID, httpx.ClientIP(r, h.cfg.TrustedProxy))
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, map[string]any{"user": u, "tempPassword": pw})
		return nil
	})
}
