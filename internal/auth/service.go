package auth

import (
	"context"
	"strings"
	"time"

	"placementhub/internal/audit"
	"placementhub/internal/config"
	"placementhub/internal/db"
	"placementhub/internal/domain"
	"placementhub/internal/httpx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool *pgxpool.Pool
	cfg  config.Config
	Now  func() time.Time
}

func NewService(pool *pgxpool.Pool, cfg config.Config) *Service {
	return &Service{pool: pool, cfg: cfg, Now: time.Now}
}

type User struct {
	ID                 uuid.UUID `json:"id"`
	Email              string    `json:"email"`
	Name               string    `json:"name"`
	Role               string    `json:"role"`
	MustChangePassword bool      `json:"mustChangePassword"`
}

type Session struct {
	User           User
	AccessToken    string
	AccessExpires  time.Time
	RefreshToken   string
	RefreshExpires time.Time
}

var errBadLogin = httpx.Unauthorized("invalid email or password")

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// Login verifies credentials and opens a new refresh-token family.
func (s *Service) Login(ctx context.Context, email, password, ua, ip string) (*Session, error) {
	email = normEmail(email)
	var (
		u    User
		hash string
		act  bool
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, name, role, must_change_password, password_hash, active FROM users WHERE lower(email) = $1`,
		email).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.MustChangePassword, &hash, &act)
	if err != nil {
		if db.IsNoRows(err) {
			// Burn comparable time so response latency does not reveal which emails exist.
			CheckPassword(string(dummyHash), password)
			return nil, errBadLogin
		}
		return nil, err
	}
	if !CheckPassword(hash, password) || !act {
		return nil, errBadLogin
	}

	var sess *Session
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, u.ID); err != nil {
			return err
		}
		var err error
		sess, err = s.issue(ctx, tx, u, uuid.New(), ua, ip)
		return err
	})
	if err != nil {
		return nil, err
	}
	audit.Log(ctx, s.pool, &u.ID, "auth.login", "user", u.ID.String(), ip, nil)
	return sess, nil
}

// issue creates an access token and a fresh refresh token in the given family.
func (s *Service) issue(ctx context.Context, q db.DBTX, u User, family uuid.UUID, ua, ip string) (*Session, error) {
	now := s.Now()
	access, accessExp, err := signAccess(s.cfg.JWTSecret, u.ID, u.Role, u.MustChangePassword, now, s.cfg.AccessTTL)
	if err != nil {
		return nil, err
	}
	refresh, hash, err := newRefreshToken()
	if err != nil {
		return nil, err
	}
	refreshExp := now.Add(s.cfg.RefreshTTL)
	if _, err := q.Exec(ctx,
		`INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at, user_agent, ip)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		u.ID, family, hash, refreshExp, truncate(ua, 300), ip); err != nil {
		return nil, err
	}
	return &Session{User: u, AccessToken: access, AccessExpires: accessExp, RefreshToken: refresh, RefreshExpires: refreshExp}, nil
}

// Refresh rotates a refresh token. Presenting an already-used token means it
// leaked, so the whole family is revoked.
func (s *Service) Refresh(ctx context.Context, token, ua, ip string) (*Session, error) {
	if token == "" {
		return nil, httpx.Unauthorized("missing refresh token")
	}
	invalid := httpx.Unauthorized("invalid or expired refresh token")
	var sess *Session
	var reuse bool

	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			id, userID, family uuid.UUID
			expires            time.Time
			revoked            *time.Time
			u                  User
			act                bool
		)
		err := tx.QueryRow(ctx,
			`SELECT rt.id, rt.user_id, rt.family_id, rt.expires_at, rt.revoked_at,
			        u.email, u.name, u.role, u.must_change_password, u.active
			   FROM refresh_tokens rt JOIN users u ON u.id = rt.user_id
			  WHERE rt.token_hash = $1 FOR UPDATE OF rt`, HashToken(token)).
			Scan(&id, &userID, &family, &expires, &revoked, &u.Email, &u.Name, &u.Role, &u.MustChangePassword, &act)
		if err != nil {
			if db.IsNoRows(err) {
				return invalid
			}
			return err
		}
		u.ID = userID
		if revoked != nil {
			reuse = true
			_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL`, family)
			if err != nil {
				return err
			}
			return nil // commit the revocation, then report invalid below
		}
		if !s.Now().Before(expires) || !act {
			return invalid
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		sess, err = s.issue(ctx, tx, u, family, ua, ip)
		return err
	})
	if err != nil {
		return nil, err
	}
	if reuse {
		audit.Log(ctx, s.pool, nil, "auth.refresh_reuse", "refresh_token", "", ip, nil)
		return nil, invalid
	}
	return sess, nil
}

// Logout revokes the refresh-token family. It is idempotent.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now()
		  WHERE revoked_at IS NULL AND family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)`,
		HashToken(token))
	return err
}

// ChangePassword verifies the current password, sets the new one and signs
// every other session out.
func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, next, ua, ip string) (*Session, error) {
	var (
		u    User
		hash string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, name, role, password_hash FROM users WHERE id = $1 AND active`, userID).
		Scan(&u.ID, &u.Email, &u.Name, &u.Role, &hash)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.Unauthorized("account not found")
		}
		return nil, err
	}
	if !CheckPassword(hash, current) {
		return nil, httpx.Unprocessable("wrong_password", "current password is incorrect")
	}
	var v httpx.V
	ValidatePassword(&v, "newPassword", next, u.Email)
	if next == current {
		v.Add("newPassword", "must differ from the current password")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	newHash, err := HashPassword(next)
	if err != nil {
		return nil, err
	}

	var sess *Session
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE users SET password_hash = $2, must_change_password = false, updated_at = now() WHERE id = $1`,
			userID, newHash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
			return err
		}
		u.MustChangePassword = false
		var err error
		sess, err = s.issue(ctx, tx, u, uuid.New(), ua, ip)
		return err
	})
	if err != nil {
		return nil, err
	}
	audit.Log(ctx, s.pool, &userID, "auth.password_changed", "user", userID.String(), ip, nil)
	return sess, nil
}

// RevokeAll signs a user out everywhere (used when an account is deactivated).
func (s *Service) RevokeAll(ctx context.Context, q db.DBTX, userID uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// GetUser loads the account behind a token.
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, name, role, must_change_password FROM users WHERE id = $1 AND active`, id).
		Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.MustChangePassword)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.Unauthorized("account not found")
		}
		return nil, err
	}
	return &u, nil
}

type CompanyRegistration struct {
	CompanyName string `json:"companyName"`
	Industry    string `json:"industry"`
	HRName      string `json:"hrName"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	Password    string `json:"password"`
}

// RegisterCompany creates a company account. It starts Pending: it may draft
// jobs but cannot submit them until the placement cell approves it.
func (s *Service) RegisterCompany(ctx context.Context, in CompanyRegistration, ip string) (*User, error) {
	in.Email = normEmail(in.Email)
	in.CompanyName = strings.TrimSpace(in.CompanyName)
	var v httpx.V
	v.Text("companyName", in.CompanyName, 2, 120)
	v.Text("industry", in.Industry, 0, 120)
	v.Text("hrName", in.HRName, 2, 120)
	v.Text("phone", in.Phone, 0, 30)
	v.Email("email", in.Email)
	ValidatePassword(&v, "password", in.Password, in.Email)
	if err := v.Err(); err != nil {
		return nil, err
	}
	hash, err := HashPassword(in.Password)
	if err != nil {
		return nil, err
	}

	var u User
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO users (email, password_hash, role, name) VALUES ($1, $2, 'company', $3)
			 RETURNING id, email, name, role, must_change_password`,
			in.Email, hash, in.HRName).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.MustChangePassword)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO companies (user_id, name, industry, hr_name, email, phone, color)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			u.ID, in.CompanyName, strings.TrimSpace(in.Industry), strings.TrimSpace(in.HRName), in.Email,
			strings.TrimSpace(in.Phone), domain.ColorFor(in.CompanyName))
		return err
	})
	if err != nil {
		if db.PgCode(err) == db.CodeUniqueViolation {
			if strings.Contains(db.PgConstraint(err), "companies_name") {
				return nil, httpx.Conflict("company_exists", "a company with this name is already registered")
			}
			return nil, httpx.Conflict("email_taken", "an account with this email already exists")
		}
		return nil, err
	}
	audit.Log(ctx, s.pool, &u.ID, "company.registered", "company", u.ID.String(), ip, map[string]any{"name": in.CompanyName})
	return &u, nil
}

// EnsureAdmin creates the bootstrap admin from config when no admin exists.
func (s *Service) EnsureAdmin(ctx context.Context) (created bool, err error) {
	if s.cfg.BootstrapAdminEmail == "" {
		return false, nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	hash, err := HashPassword(s.cfg.BootstrapAdminPassword)
	if err != nil {
		return false, err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO users (email, password_hash, role, name) VALUES ($1, $2, 'admin', $3) ON CONFLICT DO NOTHING`,
		s.cfg.BootstrapAdminEmail, hash, s.cfg.BootstrapAdminName)
	return err == nil, err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// CreateAdmin adds another placement-cell account with a temporary password
// that must be changed at first login. The password is returned once.
func (s *Service) CreateAdmin(ctx context.Context, name, email string, actor uuid.UUID, ip string) (*User, string, error) {
	email, name = normEmail(email), strings.TrimSpace(name)
	var v httpx.V
	v.Text("name", name, 2, 120)
	v.Email("email", email)
	if err := v.Err(); err != nil {
		return nil, "", err
	}
	pw, err := TempPassword()
	if err != nil {
		return nil, "", err
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return nil, "", err
	}
	u := User{Email: email, Name: name, Role: domain.RoleAdmin, MustChangePassword: true}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role, name, must_change_password) VALUES ($1, $2, 'admin', $3, true) RETURNING id`,
		email, hash, name).Scan(&u.ID)
	if err != nil {
		if db.PgCode(err) == db.CodeUniqueViolation {
			return nil, "", httpx.Conflict("email_taken", "an account with this email already exists")
		}
		return nil, "", err
	}
	audit.Log(ctx, s.pool, &actor, "admin.created", "user", u.ID.String(), ip, nil)
	return &u, pw, nil
}
