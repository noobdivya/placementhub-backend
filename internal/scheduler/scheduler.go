// Package scheduler runs the periodic jobs: closing expired postings,
// deadline / offer / drive reminders and housekeeping.
//
// Every task is idempotent (notifications carry dedupe keys), and a Postgres
// advisory lock ensures only one API instance runs a tick at a time.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"placementhub/internal/application"
	"placementhub/internal/drive"
	"placementhub/internal/email"
	"placementhub/internal/job"
	"placementhub/internal/push"
	"placementhub/internal/round"

	"github.com/jackc/pgx/v5/pgxpool"
)

const lockKey int64 = 0x504c4143454d4e54 // "PLACEMNT"

type Scheduler struct {
	pool   *pgxpool.Pool
	jobs   *job.Service
	apps   *application.Service
	drives *drive.Service
	rounds *round.Service
	Now    func() time.Time
}

func New(pool *pgxpool.Pool, jobs *job.Service, apps *application.Service, drives *drive.Service, rounds *round.Service) *Scheduler {
	return &Scheduler{pool: pool, jobs: jobs, apps: apps, drives: drives, rounds: rounds, Now: time.Now}
}

// Result reports what one tick did.
type Result struct {
	Ran               bool // false when another instance held the lock
	JobsClosed        int64
	DeadlineReminders int64
	OffersExpired     int
	OfferReminders    int
	DriveReminders    int64
	RoundReminders    int64
	Cleaned           int64
}

// Run ticks every minute until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if res, err := s.RunOnce(ctx); err != nil {
			slog.ErrorContext(ctx, "scheduler tick failed", "err", err)
		} else if res.Ran {
			slog.DebugContext(ctx, "scheduler tick", "result", res)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce executes every task once. A failing task does not stop the others.
func (s *Scheduler) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var got bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKey).Scan(&got); err != nil {
		return res, err
	}
	if !got {
		return res, nil
	}
	res.Ran = true

	var firstErr error
	note := func(name string, err error) {
		if err != nil {
			slog.ErrorContext(ctx, "scheduler task failed", "task", name, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	var err2 error
	res.JobsClosed, err2 = s.jobs.CloseExpired(ctx)
	note("close_expired_jobs", err2)
	res.DeadlineReminders, err2 = s.jobs.SendDeadlineReminders(ctx)
	note("deadline_reminders", err2)
	res.OffersExpired, err2 = s.apps.ExpireOffers(ctx)
	note("expire_offers", err2)
	res.OfferReminders, err2 = s.apps.SendOfferReminders(ctx)
	note("offer_reminders", err2)
	res.DriveReminders, err2 = s.drives.SendReminders(ctx)
	note("drive_reminders", err2)
	res.RoundReminders, err2 = s.rounds.SendReminders(ctx)
	note("round_reminders", err2)
	res.Cleaned, err2 = s.cleanup(ctx)
	note("cleanup", err2)

	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	return res, firstErr
}

// cleanup removes stale rows that only accumulate.
func (s *Scheduler) cleanup(ctx context.Context) (int64, error) {
	now := s.Now()
	var total int64
	tag, err := s.pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < $1`, now.Add(-24*time.Hour))
	if err != nil {
		return total, err
	}
	total += tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `DELETE FROM notifications WHERE read_at IS NOT NULL AND created_at < $1`, now.Add(-90*24*time.Hour))
	if err != nil {
		return total, err
	}
	total += tag.RowsAffected()
	n, err := push.Cleanup(ctx, s.pool, now.Add(-7*24*time.Hour))
	if err != nil {
		return total, err
	}
	total += n
	n, err = email.Cleanup(ctx, s.pool, now.Add(-7*24*time.Hour))
	return total + n, err
}
