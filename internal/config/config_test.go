package config

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// setEnv sets variables for one test and clears anything that could leak in.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"APP_ENV", "JWT_SECRET", "COOKIE_SECURE", "COOKIE_SAMESITE", "VAPID_PUBLIC_KEY", "VAPID_PRIVATE_KEY",
		"BOOTSTRAP_ADMIN_EMAIL", "BOOTSTRAP_ADMIN_PASSWORD", "APP_TIMEZONE", "CORS_ALLOWED_ORIGINS", "FRONTEND_URL",
		"OFFER_VALIDITY_DAYS", "MAX_UPLOAD_MB", "ACCESS_TOKEN_TTL", "RUN_WORKERS", "TRUSTED_PROXY",
		"RESEND_API_KEY", "EMAIL_FROM", "EMAIL_ROUND_REMINDER_LEAD_TIMES"} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestDevelopmentDefaults(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "development" || c.CookieSecure || c.Location.String() != "Asia/Kolkata" || c.AccessTTL != 15*time.Minute ||
		c.OfferValidity != 7*24*time.Hour || c.MaxUploadBytes() != 5<<20 || c.PushEnabled() || !c.RunWorkers {
		t.Errorf("defaults = %+v", c)
	}
	if c.EmailEnabled() {
		t.Error("email should not be enabled without RESEND_API_KEY/EMAIL_FROM")
	}
	if len(c.EmailReminderLeadTimes) != 2 || c.EmailReminderLeadTimes[0] != 24*time.Hour || c.EmailReminderLeadTimes[1] != time.Hour {
		t.Errorf("default reminder lead times = %v, want [24h 1h]", c.EmailReminderLeadTimes)
	}
	if len(c.CORSOrigins) != 1 || c.CORSOrigins[0] != "http://localhost:3000" {
		t.Errorf("CORS = %v", c.CORSOrigins)
	}
	if c.CookieSameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v", c.CookieSameSite)
	}
}

func TestProductionRefusesInsecureSettings(t *testing.T) {
	setEnv(t, map[string]string{"APP_ENV": "production"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Errorf("production without a JWT secret: %v", err)
	}
	setEnv(t, map[string]string{"APP_ENV": "production", "JWT_SECRET": "dev-only-secret-change-me-dev-only-secret"})
	if _, err := Load(); err == nil {
		t.Error("production accepted the development secret")
	}
	setEnv(t, map[string]string{"APP_ENV": "production", "JWT_SECRET": strings.Repeat("s", 40), "COOKIE_SECURE": "false"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "COOKIE_SECURE") {
		t.Errorf("production with insecure cookies: %v", err)
	}
	setEnv(t, map[string]string{"APP_ENV": "production", "JWT_SECRET": strings.Repeat("s", 40)})
	c, err := Load()
	if err != nil || !c.CookieSecure {
		t.Errorf("valid production config: %v (secure=%v)", err, c.CookieSecure)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"short secret":               {"JWT_SECRET": "short"},
		"half a VAPID pair":          {"VAPID_PUBLIC_KEY": "abc"},
		"half an email pair":         {"RESEND_API_KEY": "re_abc"},
		"bad reminder lead time":     {"EMAIL_ROUND_REMINDER_LEAD_TIMES": "24h,soon"},
		"SameSite=None over http":    {"COOKIE_SAMESITE": "none"},
		"unknown SameSite":           {"COOKIE_SAMESITE": "sometimes"},
		"bad timezone":               {"APP_TIMEZONE": "Mars/Olympus"},
		"weak bootstrap password":    {"BOOTSTRAP_ADMIN_EMAIL": "a@b.co", "BOOTSTRAP_ADMIN_PASSWORD": "short"},
		"bad duration":               {"ACCESS_TOKEN_TTL": "soon"},
		"non-numeric offer validity": {"OFFER_VALIDITY_DAYS": "many"},
		"zero upload limit":          {"MAX_UPLOAD_MB": "0"},
		"bad bool":                   {"RUN_WORKERS": "maybe"},
	}
	for name, env := range cases {
		setEnv(t, env)
		if _, err := Load(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	setEnv(t, map[string]string{"CORS_ALLOWED_ORIGINS": "https://a.example/, https://b.example ,,"})
	c, err := Load()
	if err != nil || len(c.CORSOrigins) != 2 || c.CORSOrigins[0] != "https://a.example" {
		t.Errorf("CORS parsing: %v %v", c.CORSOrigins, err)
	}
}

func TestEmailConfig(t *testing.T) {
	setEnv(t, map[string]string{"RESEND_API_KEY": "re_abc", "EMAIL_FROM": "Placement Hub <onboarding@resend.dev>",
		"EMAIL_ROUND_REMINDER_LEAD_TIMES": " 48h , 2h ,,"})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.EmailEnabled() {
		t.Error("email should be enabled once RESEND_API_KEY and EMAIL_FROM are both set")
	}
	if len(c.EmailReminderLeadTimes) != 2 || c.EmailReminderLeadTimes[0] != 48*time.Hour || c.EmailReminderLeadTimes[1] != 2*time.Hour {
		t.Errorf("reminder lead times = %v, want [48h 2h] (whitespace/empty tokens trimmed)", c.EmailReminderLeadTimes)
	}
}
