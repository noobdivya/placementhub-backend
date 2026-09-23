// Package email delivers queued notifications to students over email, via
// Resend. It mirrors internal/push's shape closely: a narrow Sender
// interface, a Worker that drains an outbox table with retry/backoff, and a
// Fake for tests.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// Message is one outgoing email.
type Message struct {
	To, ToName, Subject, HTML, Text string
}

// Sender abstracts the network call so the worker can be tested without a
// real email provider. It returns the provider's HTTP status and, on
// success, its message id (recorded for support/traceability).
type Sender interface {
	Send(ctx context.Context, msg Message) (status int, providerID string, err error)
}

const resendURL = "https://api.resend.com/emails"

// Resend sends real email via Resend's HTTP API.
type Resend struct {
	apiKey, from string
	apiURL       string // overridden by tests; production always uses resendURL
	client       *http.Client
}

func NewResend(apiKey, from string) *Resend {
	return &Resend{apiKey: apiKey, from: from, apiURL: resendURL, client: &http.Client{Timeout: 15 * time.Second}}
}

func (r *Resend) Send(ctx context.Context, msg Message) (int, string, error) {
	to := msg.To
	if msg.ToName != "" {
		to = msg.ToName + " <" + msg.To + ">"
	}
	body, err := json.Marshal(struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		HTML    string   `json:"html"`
		Text    string   `json:"text"`
	}{From: r.from, To: []string{to}, Subject: msg.Subject, HTML: msg.HTML, Text: msg.Text})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.apiURL, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &out) // best-effort; a malformed body still reports the real status
	return resp.StatusCode, out.ID, nil
}

// Fake records messages and returns scripted statuses. It exists for tests.
type Fake struct {
	mu     sync.Mutex
	Sent   []Message
	Status func(Message) (int, string, error) // nil means 200 OK
}

func (f *Fake) Send(_ context.Context, msg Message) (int, string, error) {
	f.mu.Lock()
	f.Sent = append(f.Sent, msg)
	fn := f.Status
	f.mu.Unlock()
	if fn != nil {
		return fn(msg)
	}
	return http.StatusOK, "fake-id", nil
}

func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Sent)
}
