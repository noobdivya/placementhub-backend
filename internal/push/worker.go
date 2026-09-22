package push

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxAttempts = 6
	claimLease  = 2 * time.Minute
	// clockSlack lets a row stamped by the database clock count as due even when
	// this host runs a moment behind it (the two clocks are never perfectly in step).
	clockSlack     = 2 * time.Second
	defaultBatch   = 50
	sendConcurrent = 8
)

// Worker drains push_outbox. Several instances can run at once: rows are
// claimed with FOR UPDATE SKIP LOCKED.
type Worker struct {
	pool   *pgxpool.Pool
	sender Sender
	Now    func() time.Time
	Batch  int
}

func NewWorker(pool *pgxpool.Pool, sender Sender) *Worker {
	return &Worker{pool: pool, sender: sender, Now: time.Now, Batch: defaultBatch}
}

// Run polls until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		for {
			n, err := w.ProcessOnce(ctx)
			if err != nil {
				slog.ErrorContext(ctx, "push worker", "err", err)
				break
			}
			if n < w.Batch { // queue drained
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type job struct {
	outboxID    int64
	attempts    int
	title       string
	body        string
	link        string
	tag         string
	alreadyRead bool
	subID       string
	target      Target
}

// ProcessOnce claims and sends one batch, returning how many rows it handled.
func (w *Worker) ProcessOnce(ctx context.Context) (int, error) {
	now := w.Now()
	rows, err := w.pool.Query(ctx, `
WITH due AS (
    SELECT id FROM push_outbox
     WHERE (status = 'pending' AND next_attempt_at <= $1)
        OR (status = 'processing' AND locked_until < $1)
     ORDER BY next_attempt_at, id
     LIMIT $2
     FOR UPDATE SKIP LOCKED
), claimed AS (
    UPDATE push_outbox o
       SET status = 'processing', locked_until = $3, attempts = o.attempts + 1
      FROM due WHERE o.id = due.id
    RETURNING o.id, o.attempts, o.notification_id, o.subscription_id
)
SELECT c.id, c.attempts, n.title, n.body, n.link, n.dedupe_key, n.read_at IS NOT NULL,
       ps.id::text, ps.endpoint, ps.p256dh, ps.auth
  FROM claimed c
  JOIN notifications n ON n.id = c.notification_id
  JOIN push_subscriptions ps ON ps.id = c.subscription_id`,
		now.Add(clockSlack), w.Batch, now.Add(claimLease))
	if err != nil {
		return 0, err
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.outboxID, &j.attempts, &j.title, &j.body, &j.link, &j.tag, &j.alreadyRead,
			&j.subID, &j.target.Endpoint, &j.target.P256dh, &j.target.Auth); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, sendConcurrent)
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			w.deliver(ctx, j)
		}(j)
	}
	wg.Wait()
	return len(jobs), nil
}

type payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

func (w *Worker) deliver(ctx context.Context, j job) {
	if j.alreadyRead { // the student saw it in the app; do not buzz them again
		w.finish(ctx, j, "sent", "skipped: already read")
		return
	}
	body, _ := json.Marshal(payload{Title: j.title, Body: j.body, URL: j.link, Tag: j.tag})
	status, err := w.sender.Send(ctx, j.target, body)
	switch {
	case err != nil:
		w.retryOrFail(ctx, j, "network: "+err.Error())
	case status >= 200 && status < 300:
		w.finish(ctx, j, "sent", "")
		if _, err := w.pool.Exec(ctx, `UPDATE push_subscriptions SET last_success_at = $2 WHERE id = $1::uuid`, j.subID, w.Now()); err != nil {
			slog.WarnContext(ctx, "push: update last_success_at", "err", err)
		}
	case status == 404 || status == 410:
		// The browser unsubscribed or the subscription expired: forget it.
		if _, err := w.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE id = $1::uuid`, j.subID); err != nil {
			slog.WarnContext(ctx, "push: delete dead subscription", "err", err)
		}
	case status == 429 || status >= 500:
		w.retryOrFail(ctx, j, fmt.Sprintf("push service returned %d", status))
	default: // 400/401/403/413: retrying will not help
		w.finish(ctx, j, "failed", fmt.Sprintf("push service returned %d", status))
	}
}

func (w *Worker) finish(ctx context.Context, j job, status, msg string) {
	_, err := w.pool.Exec(ctx,
		`UPDATE push_outbox SET status = $2, last_error = $3, locked_until = NULL,
		        sent_at = CASE WHEN $2 = 'sent' THEN $4::timestamptz END WHERE id = $1`,
		j.outboxID, status, msg, w.Now())
	if err != nil {
		slog.WarnContext(ctx, "push: finish", "err", err)
	}
}

func (w *Worker) retryOrFail(ctx context.Context, j job, msg string) {
	if j.attempts >= maxAttempts {
		w.finish(ctx, j, "failed", msg)
		return
	}
	next := w.Now().Add(Backoff(j.attempts))
	_, err := w.pool.Exec(ctx,
		`UPDATE push_outbox SET status = 'pending', next_attempt_at = $2, last_error = $3, locked_until = NULL WHERE id = $1`,
		j.outboxID, next, msg)
	if err != nil {
		slog.WarnContext(ctx, "push: retry", "err", err)
	}
}

// Backoff is 30s, 1m, 2m, 4m ... capped at one hour.
func Backoff(attempt int) time.Duration {
	d := 30 * time.Second << max(attempt-1, 0)
	if d > time.Hour || d <= 0 {
		return time.Hour
	}
	return d
}

// Cleanup drops finished outbox rows older than the cutoff.
func Cleanup(ctx context.Context, pool *pgxpool.Pool, olderThan time.Time) (int64, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM push_outbox WHERE status IN ('sent', 'failed') AND created_at < $1`, olderThan)
	return tag.RowsAffected(), err
}
