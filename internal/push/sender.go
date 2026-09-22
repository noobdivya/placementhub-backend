// Package push delivers queued notifications to browsers over Web Push.
package push

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Target is one browser subscription.
type Target struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// Sender abstracts the network call so the worker can be tested without a
// real push service. It returns the push service's HTTP status.
type Sender interface {
	Send(ctx context.Context, t Target, payload []byte) (status int, err error)
}

// WebPush sends real VAPID-signed messages.
type WebPush struct {
	public, private, subject string
	client                   *http.Client
}

func NewWebPush(public, private, subject string) *WebPush {
	return &WebPush{
		public: public, private: private, subject: subject,
		client: &http.Client{
			Timeout: 15 * time.Second,
			// A push service answering with a redirect could bounce us to an
			// internal address; never follow it.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (s *WebPush) Send(ctx context.Context, t Target, payload []byte) (int, error) {
	resp, err := webpush.SendNotificationWithContext(ctx, payload,
		&webpush.Subscription{Endpoint: t.Endpoint, Keys: webpush.Keys{Auth: t.Auth, P256dh: t.P256dh}},
		&webpush.Options{
			Subscriber:      s.subject,
			VAPIDPublicKey:  s.public,
			VAPIDPrivateKey: s.private,
			TTL:             24 * 60 * 60,
			Urgency:         webpush.UrgencyNormal,
			HTTPClient:      s.client,
		})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// Fake records messages and returns scripted statuses. It exists for tests.
type Fake struct {
	mu     sync.Mutex
	Sent   []Sent
	Status func(t Target) (int, error) // nil means 201 Created
}

type Sent struct {
	Target  Target
	Payload []byte
}

func (f *Fake) Send(_ context.Context, t Target, payload []byte) (int, error) {
	f.mu.Lock()
	f.Sent = append(f.Sent, Sent{t, payload})
	fn := f.Status
	f.mu.Unlock()
	if fn != nil {
		return fn(t)
	}
	return http.StatusCreated, nil
}

func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Sent)
}
