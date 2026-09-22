package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// browserKeys makes a subscription key pair the way a browser would.
func browserKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	return base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(secret)
}

func TestWebPushSenderSignsEncryptsAndSendsToThePushService(t *testing.T) {
	vapidPriv, vapidPub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	var (
		got  http.Header
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p256dh, auth := browserKeys(t)
	plain := []byte(`{"title":"New job","body":"Software Engineer at Nimbus Labs","url":"/students/jobs?job=1","tag":"job_new:1"}`)
	status, err := NewWebPush(vapidPub, vapidPriv, "mailto:placements@example.edu").
		Send(context.Background(), Target{Endpoint: srv.URL + "/push/abc", P256dh: p256dh, Auth: auth}, plain)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("Send = %d, %v", status, err)
	}
	if a := got.Get("Authorization"); !strings.HasPrefix(a, "vapid t=") || !strings.Contains(a, "k="+vapidPub) {
		t.Errorf("missing or malformed VAPID header: %q", a)
	}
	if got.Get("Content-Encoding") != "aes128gcm" || got.Get("TTL") != "86400" {
		t.Errorf("headers = %v", got)
	}
	if len(body) == 0 || bytes.Contains(body, []byte("Nimbus")) || bytes.Contains(body, []byte("title")) {
		t.Error("the payload is not encrypted")
	}
}

func TestWebPushSenderRejectsBadSubscriptionKeys(t *testing.T) {
	vapidPriv, vapidPub, _ := webpush.GenerateVAPIDKeys()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }))
	defer srv.Close()
	_, err := NewWebPush(vapidPub, vapidPriv, "mailto:a@b.co").
		Send(context.Background(), Target{Endpoint: srv.URL, P256dh: "not-a-key", Auth: "x"}, []byte("hi"))
	if err == nil {
		t.Error("a garbage subscription key produced a send")
	}
}

// A push service that answers with a redirect must not be followed: it could
// bounce the server to an internal address.
func TestWebPushSenderNeverFollowsRedirects(t *testing.T) {
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { internalHits.Add(1) }))
	defer internal.Close()
	pushSvc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/admin", http.StatusTemporaryRedirect)
	}))
	defer pushSvc.Close()

	vapidPriv, vapidPub, _ := webpush.GenerateVAPIDKeys()
	p256dh, auth := browserKeys(t)
	status, err := NewWebPush(vapidPub, vapidPriv, "mailto:a@b.co").
		Send(context.Background(), Target{Endpoint: pushSvc.URL, P256dh: p256dh, Auth: auth}, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the 307 surfaced unchanged", status)
	}
	if internalHits.Load() != 0 {
		t.Fatal("the sender followed a redirect to another host (SSRF)")
	}
}
