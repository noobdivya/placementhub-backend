package email

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testResend(srv *httptest.Server, apiKey, from string) *Resend {
	r := NewResend(apiKey, from)
	r.apiURL = srv.URL
	return r
}

func TestResendSenderPostsTheExpectedRequest(t *testing.T) {
	var (
		gotAuth, gotContentType string
		gotBody                 map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_abc123"}`))
	}))
	defer srv.Close()

	sender := testResend(srv, "re_testkey", "Placement Hub <onboarding@resend.dev>")
	status, providerID, err := sender.Send(context.Background(), Message{
		To: "student@example.edu", ToName: "Ananya Sharma", Subject: "Aptitude Test Scheduled – Nimbus Labs",
		HTML: "<p>hi</p>", Text: "hi",
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("Send = %d, %v", status, err)
	}
	if providerID != "msg_abc123" {
		t.Errorf("providerID = %q", providerID)
	}
	if gotAuth != "Bearer re_testkey" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotBody["from"] != "Placement Hub <onboarding@resend.dev>" || gotBody["subject"] != "Aptitude Test Scheduled – Nimbus Labs" {
		t.Errorf("body = %v", gotBody)
	}
	to, _ := gotBody["to"].([]any)
	if len(to) != 1 || to[0] != "Ananya Sharma <student@example.edu>" {
		t.Errorf("to = %v", gotBody["to"])
	}
}

func TestResendSenderSurfacesNonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	status, _, err := testResend(srv, "re_testkey", "test@example.edu").
		Send(context.Background(), Message{To: "a@b.co", Subject: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 surfaced unchanged (the worker decides retry policy)", status)
	}
}

func TestResendSenderOmitsToNameWhenEmpty(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, _, err := testResend(srv, "re_testkey", "test@example.edu").
		Send(context.Background(), Message{To: "a@b.co", Subject: "x"})
	if err != nil {
		t.Fatal(err)
	}
	to, _ := gotBody["to"].([]any)
	if len(to) != 1 || to[0] != "a@b.co" {
		t.Errorf("to = %v, want the bare address with no name wrapper", gotBody["to"])
	}
}
