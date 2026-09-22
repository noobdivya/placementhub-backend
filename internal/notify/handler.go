package notify

import (
	"net/http"
	"strconv"

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

// PublicRoutes: the VAPID public key is not a secret and is needed before login state is known.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/push/vapid-public-key", httpx.Handle(h.vapidKey))
}

// StudentRoutes are mounted behind Authenticate + RequireRole(student).
// Push is student-only in V1.
func (h *Handler) StudentRoutes(r chi.Router) {
	r.Get("/notifications", httpx.Handle(h.list))
	r.Get("/notifications/unread-count", httpx.Handle(h.unread))
	r.Post("/notifications/read-all", httpx.Handle(h.readAll))
	r.Post("/notifications/{id}/read", httpx.Handle(h.read))
	r.Get("/notification-preferences", httpx.Handle(h.getPrefs))
	r.Put("/notification-preferences", httpx.Handle(h.putPrefs))
	r.Post("/push-subscriptions", httpx.Handle(h.subscribe))
	r.Delete("/push-subscriptions", httpx.Handle(h.unsubscribe))
}

func (h *Handler) vapidKey(w http.ResponseWriter, r *http.Request) error {
	if !h.cfg.PushEnabled() {
		return httpx.NewError(http.StatusServiceUnavailable, "push_disabled", "web push is not configured on this server")
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"publicKey": h.cfg.VAPIDPublic})
	return nil
}

func (h *Handler) uid(r *http.Request) uuid.UUID { return auth.MustPrincipal(r).UserID }

func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 20
	}
	res, err := h.svc.List(r.Context(), h.uid(r), r.URL.Query().Get("unread") == "true", limit, r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, res)
	return nil
}

func (h *Handler) unread(w http.ResponseWriter, r *http.Request) error {
	n, err := h.svc.UnreadCount(r.Context(), h.uid(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]int{"count": n})
	return nil
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) error {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return httpx.NotFound("notification")
	}
	if err := h.svc.MarkRead(r.Context(), h.uid(r), id); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h *Handler) readAll(w http.ResponseWriter, r *http.Request) error {
	n, err := h.svc.MarkAllRead(r.Context(), h.uid(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]int64{"updated": n})
	return nil
}

func (h *Handler) getPrefs(w http.ResponseWriter, r *http.Request) error {
	p, err := h.svc.Preferences(r.Context(), h.uid(r))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, p)
	return nil
}

func (h *Handler) putPrefs(w http.ResponseWriter, r *http.Request) error {
	var in PrefUpdate
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	p, err := h.svc.UpdatePreferences(r.Context(), h.uid(r), in)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, p)
	return nil
}

func (h *Handler) subscribe(w http.ResponseWriter, r *http.Request) error {
	if !h.cfg.PushEnabled() {
		return httpx.NewError(http.StatusServiceUnavailable, "push_disabled", "web push is not configured on this server")
	}
	var in Subscription
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	if err := h.svc.Subscribe(r.Context(), h.uid(r), in, r.UserAgent()); err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, map[string]bool{"subscribed": true})
	return nil
}

func (h *Handler) unsubscribe(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Endpoint string `json:"endpoint"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	if in.Endpoint == "" {
		return httpx.BadRequest("endpoint is required")
	}
	if err := h.svc.Unsubscribe(r.Context(), h.uid(r), in.Endpoint); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}
