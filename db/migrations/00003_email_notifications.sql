-- +goose Up

-- ---------------------------------------------------------------------------
-- Email delivery. Mirrors push_outbox's claim/retry shape (see
-- internal/push/worker.go), but — unlike push, where a payload is a tiny
-- title/body/url pointer resolved by joining notifications at claim time —
-- an email needs much richer, per-event content (company, round, schedule,
-- venue, instructions). So email_outbox stores its own rendered subject/
-- body_html/body_text at insert time rather than joining notifications for
-- content at send time. It also has no jsonb column, deliberately: this
-- data is entirely plain text, avoiding the json.RawMessage vs. bare []byte
-- encoding pitfall documented in internal/notify/notifier.go and
-- internal/audit/audit.go.
--
-- One row per notification, not one row per (notification, device) the way
-- push_outbox is: a student has exactly one registered address (users.email),
-- so there is no analogue of "multiple subscribed browsers" to fan out to.
-- to_email/to_name are a snapshot taken at insert time, matching an audit
-- trail's usual semantics: this should record the address actually used even
-- if the student's registered email later changes.
CREATE TABLE email_outbox (
    id                   bigserial PRIMARY KEY,
    notification_id      uuid        NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    to_email             text        NOT NULL,
    to_name              text        NOT NULL DEFAULT '',
    subject              text        NOT NULL,
    body_html            text        NOT NULL,
    body_text            text        NOT NULL DEFAULT '',
    status               text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'sent', 'failed')),
    attempts             integer     NOT NULL DEFAULT 0,
    next_attempt_at      timestamptz NOT NULL DEFAULT now(),
    locked_until         timestamptz,
    last_error           text        NOT NULL DEFAULT '',
    provider_message_id  text        NOT NULL DEFAULT '', -- Resend's returned id, for support/traceability
    created_at           timestamptz NOT NULL DEFAULT now(),
    sent_at              timestamptz
);
CREATE INDEX email_outbox_due_idx ON email_outbox (next_attempt_at) WHERE status IN ('pending', 'processing');
CREATE INDEX email_outbox_notification_idx ON email_outbox (notification_id);

-- Master switch and per-category mute for email, mirroring push_enabled /
-- notification_preferences.push exactly. Both default true: unlike marketing
-- email, this is core hiring-workflow communication, so existing users start
-- opted in.
ALTER TABLE notification_settings ADD COLUMN email_enabled boolean NOT NULL DEFAULT true;
ALTER TABLE notification_preferences ADD COLUMN email boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE notification_preferences DROP COLUMN IF EXISTS email;
ALTER TABLE notification_settings DROP COLUMN IF EXISTS email_enabled;
DROP TABLE IF EXISTS email_outbox;
