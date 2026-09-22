// Package auth implements passwords, JWT access tokens, rotating refresh
// tokens and role-based access control.
package auth

import (
	"crypto/rand"
	"math/big"

	"placementhub/internal/httpx"

	"golang.org/x/crypto/bcrypt"
)

// BcryptCost is a variable so tests can lower it.
var BcryptCost = 12

const (
	minPasswordLen = 8
	maxPasswordLen = 72 // bcrypt ignores anything beyond 72 bytes
)

func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), BcryptCost)
	return string(b), err
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// dummyHash lets Login spend the same time whether or not the email exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("placementhub-dummy"), bcrypt.DefaultCost)

// ValidatePassword enforces the password policy.
func ValidatePassword(v *httpx.V, field, pw, email string) {
	switch {
	case len(pw) < minPasswordLen:
		v.Add(field, "must be at least 8 characters")
	case len(pw) > maxPasswordLen:
		v.Add(field, "must be at most 72 characters")
	case email != "" && pw == email:
		v.Add(field, "must not equal the email address")
	}
}

const tempAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// TempPassword returns a random 12-character password with no look-alike characters.
func TempPassword() (string, error) {
	out := make([]byte, 12)
	max := big.NewInt(int64(len(tempAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = tempAlphabet[n.Int64()]
	}
	return string(out), nil
}
