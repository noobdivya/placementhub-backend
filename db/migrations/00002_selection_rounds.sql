-- +goose Up

-- ---------------------------------------------------------------------------
-- Selection rounds: an ordered, company-defined pipeline layered on top of a
-- job (e.g. Aptitude Test, Coding Round, Technical Interview, HR Interview,
-- Final Selection). Purely additive: the existing 5-stage `applications.stage`
-- pipeline, `offers`, eligibility and reports are untouched. Clearing the
-- final round here is only a status; the recruiter still uses
-- PATCH /company/applications/{id}/stage to create the real offer.
-- ---------------------------------------------------------------------------
CREATE TABLE job_rounds (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id           uuid        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    seq              integer     NOT NULL CHECK (seq > 0),
    name             text        NOT NULL,
    mode             text        NOT NULL CHECK (mode IN ('Online', 'Offline')),
    scheduled_at     timestamptz, -- nullable: many rounds aren't dated until earlier ones finish
    duration_minutes integer     NOT NULL DEFAULT 60 CHECK (duration_minutes > 0),
    location         text        NOT NULL DEFAULT '', -- venue, or a meeting link when mode = Online
    instructions     text        NOT NULL DEFAULT '',
    version          integer     NOT NULL DEFAULT 1, -- bumped on reschedule, for notification dedupe (see drives.version)
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (job_id, seq)
);
CREATE INDEX job_rounds_job_idx ON job_rounds (job_id, seq);

-- One row per (application, round) that has ever moved off the implicit
-- 'Upcoming' default. Rows are created lazily by the recruiter's first status
-- change, not eagerly at apply time (see internal/round/service.go).
CREATE TABLE application_round_status (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid        NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    round_id       uuid        NOT NULL REFERENCES job_rounds (id) ON DELETE CASCADE,
    status         text        NOT NULL DEFAULT 'Upcoming'
        CHECK (status IN ('Upcoming', 'Scheduled', 'Cleared', 'Rejected')),
    note           text        NOT NULL DEFAULT '',
    actor_user_id  uuid REFERENCES users (id) ON DELETE SET NULL,
    version        integer     NOT NULL DEFAULT 1, -- bumped on every status change, for notification dedupe
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (application_id, round_id)
);
CREATE INDEX application_round_status_app_idx ON application_round_status (application_id);
CREATE INDEX application_round_status_round_idx ON application_round_status (round_id);

-- +goose Down
DROP TABLE IF EXISTS application_round_status, job_rounds;
