package email

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"placementhub/internal/push"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxAttempts    = 6
	claimLease     = 2 * time.Minute
	clockSlack     = 2 * time.Second
	defaultBatch   = 50
	sendConcurrent = 8
)

// Worker drains email_outbox. Several instances can run at once: rows are
// claimed with FOR UPDATE SKIP LOCKED. Retry timing reuses push.Backoff
// directly rather than duplicating it, so the two retry curves can't drift.
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
				slog.ErrorContext(ctx, "email worker", "err", err)
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
	outboxID                        int64
	attempts                        int
	to, toName, subject, html, text string
}

// ProcessOnce claims and sends one batch, returning how many rows it handled.
func (w *Worker) ProcessOnce(ctx context.Context) (int, error) {
	now := w.Now()
	rows, err := w.pool.Query(ctx, `
WITH due AS (
    SELECT id FROM email_outbox
     WHERE (status = 'pending' AND next_attempt_at <= $1)
        OR (status = 'processing' AND locked_until < $1)
     ORDER BY next_attempt_at, id
     LIMIT $2
     FOR UPDATE SKIP LOCKED
), claimed AS (
    UPDATE email_outbox o
       SET status = 'processing', locked_until = $3, attempts = o.attempts + 1
      FROM due WHERE o.id = due.id
    RETURNING o.id, o.attempts, o.to_email, o.to_name, o.subject, o.body_html, o.body_text
)
SELECT id, attempts, to_email, to_name, subject, body_html, body_text FROM claimed`,
		now.Add(clockSlack), w.Batch, now.Add(claimLease))
	if err != nil {
		return 0, err
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.outboxID, &j.attempts, &j.to, &j.toName, &j.subject, &j.html, &j.text); err != nil {
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

func (w *Worker) deliver(ctx context.Context, j job) {
	status, providerID, err := w.sender.Send(ctx, Message{To: j.to, ToName: j.toName, Subject: j.subject, HTML: j.html, Text: j.text})
	switch {
	case err != nil:
		w.retryOrFail(ctx, j, "network: "+err.Error())
	case status >= 200 && status < 300:
		w.finish(ctx, j, "sent", "", providerID)
	case status == 429 || status >= 500:
		w.retryOrFail(ctx, j, fmt.Sprintf("resend returned %d", status))
	default: // 400/401/403/422: retrying will not help
		w.finish(ctx, j, "failed", fmt.Sprintf("resend returned %d", status), "")
	}
}

func (w *Worker) finish(ctx context.Context, j job, status, msg, providerID string) {
	_, err := w.pool.Exec(ctx,
		`UPDATE email_outbox SET status = $2, last_error = $3, provider_message_id = $5, locked_until = NULL,
		        sent_at = CASE WHEN $2 = 'sent' THEN $4::timestamptz END WHERE id = $1`,
		j.outboxID, status, msg, w.Now(), providerID)
	if err != nil {
		slog.WarnContext(ctx, "email: finish", "err", err)
	}
}

func (w *Worker) retryOrFail(ctx context.Context, j job, msg string) {
	if j.attempts >= maxAttempts {
		w.finish(ctx, j, "failed", msg, "")
		return
	}
	next := w.Now().Add(push.Backoff(j.attempts))
	_, err := w.pool.Exec(ctx,
		`UPDATE email_outbox SET status = 'pending', next_attempt_at = $2, last_error = $3, locked_until = NULL WHERE id = $1`,
		j.outboxID, next, msg)
	if err != nil {
		slog.WarnContext(ctx, "email: retry", "err", err)
	}
}

// Cleanup drops finished outbox rows older than the cutoff.
func Cleanup(ctx context.Context, pool *pgxpool.Pool, olderThan time.Time) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM email_outbox WHERE status IN ('sent', 'failed') AND created_at < $1`, olderThan)
	return tag.RowsAffected(), err
}
