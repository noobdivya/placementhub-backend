package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const issuer = "placementhub"

// Claims are the access-token claims. MustChange mirrors users.must_change_password.
type Claims struct {
	Role       string `json:"role"`
	MustChange bool   `json:"mcp,omitempty"`
	jwt.RegisteredClaims
}

func signAccess(secret []byte, userID uuid.UUID, role string, mustChange bool, now time.Time, ttl time.Duration) (string, time.Time, error) {
	exp := now.Add(ttl)
	claims := Claims{
		Role:       role,
		MustChange: mustChange,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.NewString(),
		},
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	return s, exp, err
}

// ParseAccess validates signature, algorithm, issuer and expiry.
func ParseAccess(secret []byte, token string, now time.Time) (*Claims, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return secret, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(issuer),
		jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(c.Subject); err != nil {
		return nil, errors.New("bad subject")
	}
	return &c, nil
}

// newRefreshToken returns an opaque random token and the hash we store.
func newRefreshToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken is the storage form of a refresh token; the raw value is never stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
