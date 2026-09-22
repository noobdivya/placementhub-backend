// Package notify creates in-app notifications and queues Web Push deliveries.
package notify

import (
	"context"
	"encoding/json"
	"fmt"

	"placementhub/internal/db"
	"placementhub/internal/domain"

	"github.com/google/uuid"
)

// Spec describes one notification. Every recipient receives the same content.
type Spec struct {
	Type      string
	Title     string
	Body      string
	Link      string
	Data      map[string]any
	DedupeKey string // unique per recipient; re-sending the same key is a no-op
}

// Notifier writes notifications inside the caller's transaction, so a
// notification exists if and only if the change that caused it committed.
type Notifier struct {
	pushEnabled bool
}

func New(pushEnabled bool) *Notifier { return &Notifier{pushEnabled: pushEnabled} }

// ToUser notifies one user. It reports whether a new notification was created.
func (n *Notifier) ToUser(ctx context.Context, q db.DBTX, userID uuid.UUID, s Spec) (bool, error) {
	c, err := n.ToUsers(ctx, q, `SELECT $1::uuid AS user_id`, []any{userID}, s)
	return c > 0, err
}

// ToUsers notifies every user returned by recipientsSQL, a query that yields a
// single `user_id` column and uses $1..$len(args) for its own parameters.
// It returns how many notifications were created.
//
// One statement inserts the inbox rows (skipping duplicates by dedupe key) and
// queues a push per device for recipients who allow it:
//   - the master push switch is on (default on), and
//   - the category is not muted (critical interviews/offers ignore per-category mutes).
//
// Muted recipients still get the inbox row; only the push is withheld.
func (n *Notifier) ToUsers(ctx context.Context, q db.DBTX, recipientsSQL string, args []any, s Spec) (int64, error) {
	if s.Type == "" || s.DedupeKey == "" || s.Title == "" {
		return 0, fmt.Errorf("notify: type, title and dedupe key are required")
	}
	data := s.Data
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	k := len(args)
	stmt := fmt.Sprintf(`
WITH recipients AS (%[1]s),
ins AS (
    INSERT INTO notifications (user_id, type, category, title, body, link, data, dedupe_key)
    SELECT r.user_id, $%[2]d::text, $%[3]d::text, $%[4]d::text, $%[5]d::text, $%[6]d::text, $%[7]d::jsonb, $%[8]d::text
      FROM recipients r
    ON CONFLICT (user_id, dedupe_key) DO NOTHING
    RETURNING id, user_id
),
queued AS (
    INSERT INTO push_outbox (notification_id, subscription_id)
    SELECT ins.id, ps.id
      FROM ins
      JOIN push_subscriptions ps ON ps.user_id = ins.user_id
      LEFT JOIN notification_settings ns ON ns.user_id = ins.user_id
      LEFT JOIN notification_preferences np ON np.user_id = ins.user_id AND np.category = $%[3]d::text
     WHERE $%[9]d::boolean
       AND COALESCE(ns.push_enabled, true)
       AND ($%[3]d::text = 'critical' OR COALESCE(np.push, true))
    RETURNING 1
)
SELECT count(*) FROM ins`, recipientsSQL, k+1, k+2, k+3, k+4, k+5, k+6, k+7, k+8)

	all := append(append([]any{}, args...),
		s.Type, domain.CategoryFor(s.Type), s.Title, s.Body, s.Link, raw, s.DedupeKey, n.pushEnabled)

	var count int64
	if err := q.QueryRow(ctx, stmt, all...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
