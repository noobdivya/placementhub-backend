package auth

import (
	"strings"
	"testing"
	"time"

	"placementhub/internal/httpx"

	"github.com/google/uuid"
)

func init() { BcryptCost = 4 }

func TestPasswordHashingAndPolicy(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if h == "correct horse battery" || !strings.HasPrefix(h, "$2") {
		t.Errorf("password not hashed with bcrypt: %q", h)
	}
	if !CheckPassword(h, "correct horse battery") || CheckPassword(h, "wrong") || CheckPassword("garbage", "x") {
		t.Error("CheckPassword misbehaves")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Error("hashes are not salted")
	}

	policy := map[string]bool{
		"12345678": true, "short": false, "": false, strings.Repeat("a", 72): true, strings.Repeat("a", 73): false,
	}
	for pw, ok := range policy {
		var v httpx.V
		ValidatePassword(&v, "pw", pw, "a@b.co")
		if (v.Err() == nil) != ok {
			t.Errorf("policy(%d chars) valid=%v, want %v", len(pw), v.Err() == nil, ok)
		}
	}
	var v httpx.V
	ValidatePassword(&v, "pw", "me@college.edu", "me@college.edu")
	if v.Err() == nil {
		t.Error("password equal to the email accepted")
	}
}

func TestTempPasswordsAreRandomAndUnambiguous(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, err := TempPassword()
		if err != nil || len(p) != 12 {
			t.Fatalf("TempPassword = %q, %v", p, err)
		}
		if strings.ContainsAny(p, "0O1lI") {
			t.Fatalf("look-alike character in %q", p)
		}
		if seen[p] {
			t.Fatal("duplicate temporary password")
		}
		seen[p] = true
		var v httpx.V
		ValidatePassword(&v, "pw", p, "")
		if v.Err() != nil {
			t.Fatalf("generated password fails the policy: %q", p)
		}
	}
}

func TestAccessTokenRoundTripAndExpiry(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	id := uuid.New()
	now := time.Now()
	tok, exp, err := signAccess(secret, id, "student", true, now, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(now.Add(15 * time.Minute)) {
		t.Errorf("exp = %v", exp)
	}
	c, err := ParseAccess(secret, tok, now.Add(time.Minute))
	if err != nil || c.Subject != id.String() || c.Role != "student" || !c.MustChange {
		t.Fatalf("claims = %+v, %v", c, err)
	}
	if _, err := ParseAccess(secret, tok, now.Add(16*time.Minute)); err == nil {
		t.Error("expired token accepted")
	}
	if _, err := ParseAccess([]byte("another-secret-another-secret-00"), tok, now); err == nil {
		t.Error("token verified with the wrong secret")
	}
	if _, err := ParseAccess(secret, tok[:len(tok)-3]+"abc", now); err == nil {
		t.Error("tampered signature accepted")
	}
	if _, err := ParseAccess(secret, "", now); err == nil {
		t.Error("empty token accepted")
	}
}

func TestRefreshTokensAreRandomAndOnlyHashesAreStored(t *testing.T) {
	a, ha, _ := newRefreshToken()
	b, hb, _ := newRefreshToken()
	if a == b || ha == hb {
		t.Fatal("refresh tokens repeat")
	}
	if len(a) < 40 {
		t.Errorf("token too short: %d", len(a))
	}
	if HashToken(a) != ha || ha == a || len(ha) != 64 {
		t.Errorf("hash mismatch or stored in clear")
	}
}
