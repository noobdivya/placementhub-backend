// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env         string // development | production | test
	HTTPAddr    string
	DatabaseURL string

	JWTSecret      []byte
	AccessTTL      time.Duration
	RefreshTTL     time.Duration
	CookieSecure   bool
	CookieSameSite http.SameSite
	CORSOrigins    []string
	FrontendURL    string
	TrustedProxy   bool // honour X-Forwarded-For for client IPs
	UploadDir      string
	MaxUploadMB    int64
	Location       *time.Location
	OfferValidity  time.Duration

	VAPIDPublic  string
	VAPIDPrivate string
	VAPIDSubject string

	ResendAPIKey           string
	EmailFrom              string
	EmailReminderLeadTimes []time.Duration

	BootstrapAdminEmail    string
	BootstrapAdminPassword string
	BootstrapAdminName     string

	RunWorkers bool
}

func (c Config) IsProd() bool          { return c.Env == "production" }
func (c Config) PushEnabled() bool     { return c.VAPIDPublic != "" && c.VAPIDPrivate != "" }
func (c Config) EmailEnabled() bool    { return c.ResendAPIKey != "" && c.EmailFrom != "" }
func (c Config) MaxUploadBytes() int64 { return c.MaxUploadMB << 20 }

func Load() (Config, error) {
	c := Config{
		Env:         get("APP_ENV", "development"),
		HTTPAddr:    get("HTTP_ADDR", ":8080"),
		DatabaseURL: get("DATABASE_URL", "postgres://placementhub:placementhub@localhost:5433/placementhub?sslmode=disable"),
		FrontendURL: strings.TrimRight(get("FRONTEND_URL", "http://localhost:3000"), "/"),
		UploadDir:   get("UPLOAD_DIR", "./uploads"),

		VAPIDPublic:  os.Getenv("VAPID_PUBLIC_KEY"),
		VAPIDPrivate: os.Getenv("VAPID_PRIVATE_KEY"),
		VAPIDSubject: get("VAPID_SUBJECT", "mailto:placements@example.edu"),

		ResendAPIKey: os.Getenv("RESEND_API_KEY"),
		EmailFrom:    os.Getenv("EMAIL_FROM"),

		BootstrapAdminEmail:    strings.ToLower(strings.TrimSpace(os.Getenv("BOOTSTRAP_ADMIN_EMAIL"))),
		BootstrapAdminPassword: os.Getenv("BOOTSTRAP_ADMIN_PASSWORD"),
		BootstrapAdminName:     get("BOOTSTRAP_ADMIN_NAME", "Placement Officer"),
	}

	secret := os.Getenv("JWT_SECRET")
	if secret == "" && !c.IsProd() {
		secret = "dev-only-secret-change-me-dev-only-secret"
	}
	c.JWTSecret = []byte(secret)

	var err error
	if c.AccessTTL, err = duration("ACCESS_TOKEN_TTL", 15*time.Minute); err != nil {
		return c, err
	}
	if c.RefreshTTL, err = duration("REFRESH_TOKEN_TTL", 14*24*time.Hour); err != nil {
		return c, err
	}
	if c.EmailReminderLeadTimes, err = durationList("EMAIL_ROUND_REMINDER_LEAD_TIMES", []time.Duration{24 * time.Hour, time.Hour}); err != nil {
		return c, err
	}
	days, err := intVar("OFFER_VALIDITY_DAYS", 7)
	if err != nil {
		return c, err
	}
	c.OfferValidity = time.Duration(days) * 24 * time.Hour
	if c.MaxUploadMB, err = int64Var("MAX_UPLOAD_MB", 5); err != nil {
		return c, err
	}
	if c.CookieSecure, err = boolVar("COOKIE_SECURE", c.IsProd()); err != nil {
		return c, err
	}
	switch strings.ToLower(get("COOKIE_SAMESITE", "lax")) {
	case "lax":
		c.CookieSameSite = http.SameSiteLaxMode
	case "strict":
		c.CookieSameSite = http.SameSiteStrictMode
	case "none":
		c.CookieSameSite = http.SameSiteNoneMode
	default:
		return c, errors.New("COOKIE_SAMESITE must be lax, strict or none")
	}
	if c.TrustedProxy, err = boolVar("TRUSTED_PROXY", false); err != nil {
		return c, err
	}
	if c.RunWorkers, err = boolVar("RUN_WORKERS", true); err != nil {
		return c, err
	}

	origins := get("CORS_ALLOWED_ORIGINS", c.FrontendURL)
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}

	tz := get("APP_TIMEZONE", "Asia/Kolkata")
	if c.Location, err = time.LoadLocation(tz); err != nil {
		return c, fmt.Errorf("APP_TIMEZONE %q: %w", tz, err)
	}

	return c, c.validate()
}

func (c Config) validate() error {
	var errs []error
	if len(c.JWTSecret) < 32 {
		errs = append(errs, errors.New("JWT_SECRET must be set and at least 32 bytes"))
	}
	if c.IsProd() {
		if !c.CookieSecure {
			errs = append(errs, errors.New("COOKIE_SECURE must be true in production"))
		}
		if strings.Contains(string(c.JWTSecret), "dev-only") {
			errs = append(errs, errors.New("JWT_SECRET must not be the development default in production"))
		}
	}
	if c.CookieSameSite == http.SameSiteNoneMode && !c.CookieSecure {
		errs = append(errs, errors.New("COOKIE_SAMESITE=none requires COOKIE_SECURE=true"))
	}
	if (c.VAPIDPublic == "") != (c.VAPIDPrivate == "") {
		errs = append(errs, errors.New("VAPID_PUBLIC_KEY and VAPID_PRIVATE_KEY must be set together"))
	}
	if (c.ResendAPIKey == "") != (c.EmailFrom == "") {
		errs = append(errs, errors.New("RESEND_API_KEY and EMAIL_FROM must be set together"))
	}
	if c.BootstrapAdminEmail != "" && len(c.BootstrapAdminPassword) < 10 {
		errs = append(errs, errors.New("BOOTSTRAP_ADMIN_PASSWORD must be at least 10 characters"))
	}
	return errors.Join(errs...)
}

func get(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func duration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

// durationList parses a comma-separated list of durations (e.g. "24h,1h"),
// trimming whitespace around each token and skipping empty ones — the same
// shape as CORS_ALLOWED_ORIGINS's comma-split, adapted for durations instead
// of URLs.
func durationList(key string, def []time.Duration) ([]time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	var out []time.Duration
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		d, err := time.ParseDuration(tok)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		out = append(out, d)
	}
	return out, nil
}

func intVar(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

func int64Var(key string, def int64) (int64, error) {
	n, err := intVar(key, int(def))
	return int64(n), err
}

func boolVar(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return b, nil
}
