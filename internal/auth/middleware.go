package auth

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"placementhub/internal/httpx"

	"github.com/google/uuid"
)

// Principal is the authenticated caller.
type Principal struct {
	UserID     uuid.UUID
	Role       string
	MustChange bool
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// MustPrincipal is for handlers behind Authenticate.
func MustPrincipal(r *http.Request) Principal {
	p, _ := PrincipalFrom(r.Context())
	return p
}

// Authenticate requires a valid Bearer access token.
func Authenticate(secret []byte, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || token == "" {
				httpx.WriteError(w, r, httpx.Unauthorized("missing bearer token"))
				return
			}
			claims, err := ParseAccess(secret, token, now())
			if err != nil {
				httpx.WriteError(w, r, httpx.Unauthorized("invalid or expired token"))
				return
			}
			uid, _ := uuid.Parse(claims.Subject)
			p := Principal{UserID: uid, Role: claims.Role, MustChange: claims.MustChange}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// RequireRole allows only the listed roles. Accounts holding a temporary
// password must change it first.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				httpx.WriteError(w, r, httpx.Unauthorized("not authenticated"))
				return
			}
			if !slices.Contains(roles, p.Role) {
				httpx.WriteError(w, r, httpx.Forbidden("you do not have access to this resource"))
				return
			}
			if p.MustChange {
				httpx.WriteError(w, r, httpx.NewError(http.StatusForbidden, "password_change_required",
					"change your temporary password before continuing"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
