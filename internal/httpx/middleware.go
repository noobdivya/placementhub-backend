package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type ctxKey int

const requestIDKey ctxKey = 1

func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}

// WithRequestID tags each request with an id (honouring a sane inbound one).
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if len(id) == 0 || len(id) > 64 {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// AccessLog logs one line per request.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.InfoContext(r.Context(), "request",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "request_id", RequestID(r.Context()))
	})
}

// Recover turns panics into 500s instead of killing the connection.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.ErrorContext(r.Context(), "panic", "panic", rec, "stack", string(debug.Stack()),
					"request_id", RequestID(r.Context()))
				WriteError(w, r, NewError(http.StatusInternalServerError, "internal", "something went wrong"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders sets conservative defaults for an API.
func SecurityHeaders(hsts bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cross-Origin-Resource-Policy", "same-site")
			if hsts {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORS allows only the listed origins, with credentials (refresh cookie).
func CORS(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		set[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			w.Header().Add("Vary", "Origin")
			if origin != "" && set[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Expose-Headers", "X-Request-ID, Content-Disposition")
				if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
					h.Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
					h.Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
					h.Set("Access-Control-Max-Age", "600")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP returns the caller's address. Forwarded headers are only trusted
// when the deployment sits behind a known proxy.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Limiter is a keyed token-bucket limiter with idle-entry cleanup.
type Limiter struct {
	mu    sync.Mutex
	items map[string]*entry
	rate  rate.Limit
	burst int
}

type entry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewLimiter allows `burst` requests at once, refilling perMinute per minute.
func NewLimiter(perMinute float64, burst int) *Limiter {
	l := &Limiter{items: map[string]*entry{}, rate: rate.Limit(perMinute / 60), burst: burst}
	go l.gc()
	return l
}

func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.items[key]
	if !ok {
		e = &entry{lim: rate.NewLimiter(l.rate, l.burst)}
		l.items[key] = e
	}
	e.seen = time.Now()
	return e.lim.Allow()
}

func (l *Limiter) gc() {
	for range time.Tick(5 * time.Minute) {
		l.mu.Lock()
		for k, e := range l.items {
			if time.Since(e.seen) > 15*time.Minute {
				delete(l.items, k)
			}
		}
		l.mu.Unlock()
	}
}

// Limit rejects requests over the per-IP budget with 429.
func (l *Limiter) Limit(trustProxy bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(ClientIP(r, trustProxy)) {
				w.Header().Set("Retry-After", "30")
				WriteError(w, r, NewError(http.StatusTooManyRequests, "rate_limited", "too many requests, slow down"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
