// Package audit records security-relevant and administrative actions.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"

	"placementhub/internal/db"

	"github.com/google/uuid"
)

// Log writes one audit row. Failures are logged, never returned: auditing must
// not break the action it describes.
func Log(ctx context.Context, q db.DBTX, actor *uuid.UUID, action, entity, entityID, ip string, meta map[string]any) {
	if meta == nil {
		meta = map[string]any{}
	}
	raw, _ := json.Marshal(meta)
	_, err := q.Exec(ctx,
		`INSERT INTO audit_log (actor_user_id, action, entity, entity_id, meta, ip) VALUES ($1, $2, $3, $4, $5, $6)`,
		// json.RawMessage, not the bare []byte json.Marshal returns: pgx's default
		// encoding for a plain []byte is bytea, and under QueryExecModeExec (see
		// internal/db.Connect) there's no Describe step to correct that from the
		// meta column's real jsonb type — Postgres then rejects the hex-escaped
		// bytea text as invalid JSON (SQLSTATE 22P02). json.RawMessage has a
		// default mapping straight to json/jsonb.
		actor, action, entity, entityID, json.RawMessage(raw), ip)
	if err != nil {
		slog.WarnContext(ctx, "audit write failed", "action", action, "err", err)
	}
}
