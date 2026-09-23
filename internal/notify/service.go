package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"placementhub/internal/db"
	"placementhub/internal/domain"
	"placementhub/internal/httpx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AllowedPushHosts limits where the server will POST push messages. The
// endpoint is client-supplied, so without this it would be an SSRF vector.
// Entries starting with "." match any subdomain.
var AllowedPushHosts = []string{
	"fcm.googleapis.com",                // Chrome, Edge, Opera, Brave, Android
	"updates.push.services.mozilla.com", // Firefox
	".push.services.mozilla.com",
	".push.apple.com", // Safari / iOS web apps
	".notify.windows.com",
}

const maxSubscriptionsPerUser = 10

type Service struct {
	pool         *pgxpool.Pool
	pushEnabled  bool
	emailEnabled bool
}

func NewService(pool *pgxpool.Pool, pushEnabled, emailEnabled bool) *Service {
	return &Service{pool: pool, pushEnabled: pushEnabled, emailEnabled: emailEnabled}
}

type Item struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Category  string          `json:"category"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Link      string          `json:"link"`
	Data      json.RawMessage `json:"data"`
	Read      bool            `json:"read"`
	CreatedAt time.Time       `json:"createdAt"`
}

type ListResult struct {
	Items      []Item  `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// List returns a user's inbox newest-first using keyset pagination.
func (s *Service) List(ctx context.Context, userID uuid.UUID, unreadOnly bool, limit int, cursor string) (*ListResult, error) {
	var (
		curTime *time.Time
		curID   *uuid.UUID
	)
	if cursor != "" {
		t, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, httpx.BadRequest("invalid cursor")
		}
		curTime, curID = &t, &id
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, type, category, title, body, link, data, read_at IS NOT NULL, created_at
		   FROM notifications
		  WHERE user_id = $1
		    AND (NOT $2 OR read_at IS NULL)
		    AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
		  ORDER BY created_at DESC, id DESC
		  LIMIT $5`, userID, unreadOnly, curTime, curID, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := &ListResult{Items: []Item{}}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Type, &it.Category, &it.Title, &it.Body, &it.Link, &it.Data, &it.Read, &it.CreatedAt); err != nil {
			return nil, err
		}
		res.Items = append(res.Items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(res.Items) > limit {
		res.Items = res.Items[:limit]
		last := res.Items[limit-1]
		c := encodeCursor(last.CreatedAt, last.ID)
		res.NextCursor = &c
	}
	return res, nil
}

func encodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(c string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	ts, ids, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, errBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(ids)
	return t, id, err
}

var errBadCursor = httpx.BadRequest("invalid cursor")

func (s *Service) UnreadCount(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, userID).Scan(&n)
	return n, err
}

// MarkRead marks one notification read. It 404s for someone else's notification.
func (s *Service) MarkRead(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE notifications SET read_at = COALESCE(read_at, now()) WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("notification")
	}
	return nil
}

func (s *Service) MarkAllRead(ctx context.Context, userID uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL`, userID)
	return tag.RowsAffected(), err
}

// ---- preferences ----------------------------------------------------------

type CategoryPref struct {
	Category string `json:"category"`
	Label    string `json:"label"`
	Push     bool   `json:"push"`
	Email    bool   `json:"email"`
	Mutable  bool   `json:"mutable"`
}

type Preferences struct {
	PushAvailable  bool           `json:"pushAvailable"`  // server has VAPID keys configured
	PushEnabled    bool           `json:"pushEnabled"`    // master switch
	EmailAvailable bool           `json:"emailAvailable"` // server has RESEND_API_KEY/EMAIL_FROM configured
	EmailEnabled   bool           `json:"emailEnabled"`   // master switch
	Devices        int            `json:"devices"`
	Categories     []CategoryPref `json:"categories"`
}

var categoryLabels = map[string]string{
	domain.CatNewJob:       "New jobs I'm eligible for",
	domain.CatApplications: "Application updates",
	domain.CatDeadlines:    "Deadline reminders",
	domain.CatDrives:       "Drives",
	domain.CatNotices:      "Placement cell notices",
	domain.CatCritical:     "Interviews & offers",
}

func (s *Service) Preferences(ctx context.Context, userID uuid.UUID) (*Preferences, error) {
	p := &Preferences{PushAvailable: s.pushEnabled, PushEnabled: true, EmailAvailable: s.emailEnabled, EmailEnabled: true}
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT push_enabled FROM notification_settings WHERE user_id = $1), true),
		        COALESCE((SELECT email_enabled FROM notification_settings WHERE user_id = $1), true),
		        (SELECT count(*) FROM push_subscriptions WHERE user_id = $1)`, userID).
		Scan(&p.PushEnabled, &p.EmailEnabled, &p.Devices)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT category, push, email FROM notification_preferences WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pref struct{ push, email bool }
	set := map[string]pref{}
	for rows.Next() {
		var c string
		var p pref
		if err := rows.Scan(&c, &p.push, &p.email); err != nil {
			return nil, err
		}
		set[c] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range domain.MutableCategories {
		p2, ok := set[c]
		if !ok {
			p2 = pref{push: true, email: true}
		}
		p.Categories = append(p.Categories, CategoryPref{Category: c, Label: categoryLabels[c], Push: p2.push, Email: p2.email, Mutable: true})
	}
	p.Categories = append(p.Categories, CategoryPref{
		Category: domain.CatCritical, Label: categoryLabels[domain.CatCritical], Push: true, Email: true, Mutable: false})
	return p, nil
}

type PrefUpdate struct {
	PushEnabled     *bool           `json:"pushEnabled"`
	EmailEnabled    *bool           `json:"emailEnabled"`
	Categories      map[string]bool `json:"categories"`      // push mute per category
	EmailCategories map[string]bool `json:"emailCategories"` // email mute per category
}

func (s *Service) UpdatePreferences(ctx context.Context, userID uuid.UUID, in PrefUpdate) (*Preferences, error) {
	var v httpx.V
	for c := range in.Categories {
		if c == domain.CatCritical {
			v.Add("categories."+c, "interviews and offers cannot be muted individually; use the master switch")
		} else if !contains(domain.MutableCategories, c) {
			v.Add("categories."+c, "unknown category")
		}
	}
	for c := range in.EmailCategories {
		if c == domain.CatCritical {
			v.Add("emailCategories."+c, "interviews and offers cannot be muted individually; use the master switch")
		} else if !contains(domain.MutableCategories, c) {
			v.Add("emailCategories."+c, "unknown category")
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if in.PushEnabled != nil || in.EmailEnabled != nil {
			push, email := true, true
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE((SELECT push_enabled FROM notification_settings WHERE user_id = $1), true),
				        COALESCE((SELECT email_enabled FROM notification_settings WHERE user_id = $1), true)`, userID).
				Scan(&push, &email); err != nil {
				return err
			}
			if in.PushEnabled != nil {
				push = *in.PushEnabled
			}
			if in.EmailEnabled != nil {
				email = *in.EmailEnabled
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO notification_settings (user_id, push_enabled, email_enabled) VALUES ($1, $2, $3)
				 ON CONFLICT (user_id) DO UPDATE SET push_enabled = EXCLUDED.push_enabled, email_enabled = EXCLUDED.email_enabled, updated_at = now()`,
				userID, push, email); err != nil {
				return err
			}
		}
		for c, push := range in.Categories {
			if _, err := tx.Exec(ctx,
				`INSERT INTO notification_preferences (user_id, category, push) VALUES ($1, $2, $3)
				 ON CONFLICT (user_id, category) DO UPDATE SET push = EXCLUDED.push`, userID, c, push); err != nil {
				return err
			}
		}
		for c, emailOn := range in.EmailCategories {
			if _, err := tx.Exec(ctx,
				`INSERT INTO notification_preferences (user_id, category, email) VALUES ($1, $2, $3)
				 ON CONFLICT (user_id, category) DO UPDATE SET email = EXCLUDED.email`, userID, c, emailOn); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Preferences(ctx, userID)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- push subscriptions ---------------------------------------------------

type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Subscribe registers (or re-registers) a browser for push. The body is the
// PushSubscription.toJSON() shape the browser produces.
func (s *Service) Subscribe(ctx context.Context, userID uuid.UUID, sub Subscription, userAgent string) error {
	var v httpx.V
	if err := validEndpoint(sub.Endpoint); err != nil {
		v.Add("endpoint", err.Error())
	}
	if !b64Len(sub.Keys.P256dh, 60, 100) {
		v.Add("keys.p256dh", "is invalid")
	}
	if !b64Len(sub.Keys.Auth, 12, 40) {
		v.Add("keys.auth", "is invalid")
	}
	if err := v.Err(); err != nil {
		return err
	}
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// The same browser may have been registered by another account; the
		// endpoint is unique, so it moves to whoever is signed in now.
		if _, err := tx.Exec(ctx,
			`INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, user_agent)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (endpoint) DO UPDATE
			   SET user_id = EXCLUDED.user_id, p256dh = EXCLUDED.p256dh,
			       auth = EXCLUDED.auth, user_agent = EXCLUDED.user_agent`,
			userID, sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth, userAgent); err != nil {
			return err
		}
		// Keep only the newest few devices per user.
		_, err := tx.Exec(ctx,
			`DELETE FROM push_subscriptions WHERE user_id = $1 AND id NOT IN (
			   SELECT id FROM push_subscriptions WHERE user_id = $1 ORDER BY created_at DESC, id LIMIT $2)`,
			userID, maxSubscriptionsPerUser)
		return err
	})
}

// Unsubscribe removes one device. It is idempotent.
func (s *Service) Unsubscribe(ctx context.Context, userID uuid.UUID, endpoint string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE user_id = $1 AND endpoint = $2`, userID, endpoint)
	return err
}

func validEndpoint(raw string) error {
	if len(raw) == 0 || len(raw) > 2048 {
		return errString("is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Port() != "" && u.Port() != "443" {
		return errString("must be an https push service URL")
	}
	host := strings.ToLower(u.Hostname())
	for _, allowed := range AllowedPushHosts {
		if host == allowed || strings.HasPrefix(allowed, ".") && strings.HasSuffix(host, allowed) {
			return nil
		}
	}
	return errString("is not a recognised browser push service")
}

type errString string

func (e errString) Error() string { return string(e) }

func b64Len(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	return err == nil
}
