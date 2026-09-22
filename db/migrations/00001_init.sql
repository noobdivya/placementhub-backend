-- +goose Up

-- ---------------------------------------------------------------------------
-- Identity
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email                text        NOT NULL,
    password_hash        text        NOT NULL,
    role                 text        NOT NULL CHECK (role IN ('student', 'company', 'admin')),
    name                 text        NOT NULL,
    active               boolean     NOT NULL DEFAULT true,
    must_change_password boolean     NOT NULL DEFAULT false,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    last_login_at        timestamptz
);
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id   uuid        NOT NULL,
    token_hash  text        NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    user_agent  text        NOT NULL DEFAULT '',
    ip          text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);

-- ---------------------------------------------------------------------------
-- Students, companies
-- ---------------------------------------------------------------------------
CREATE TABLE students (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid          NOT NULL UNIQUE REFERENCES users (id) ON DELETE CASCADE,
    roll       text          NOT NULL UNIQUE,
    branch     text          NOT NULL,
    year       text          NOT NULL DEFAULT 'Final year',
    cgpa       numeric(4, 2) NOT NULL CHECK (cgpa >= 0 AND cgpa <= 10),
    backlogs   integer       NOT NULL DEFAULT 0 CHECK (backlogs >= 0),
    tenth      numeric(5, 2) CHECK (tenth >= 0 AND tenth <= 100),
    twelfth    numeric(5, 2) CHECK (twelfth >= 0 AND twelfth <= 100),
    phone      text          NOT NULL DEFAULT '',
    github     text          NOT NULL DEFAULT '',
    linkedin   text          NOT NULL DEFAULT '',
    about      text          NOT NULL DEFAULT '',
    skills     text[]        NOT NULL DEFAULT '{}',
    created_at timestamptz   NOT NULL DEFAULT now(),
    updated_at timestamptz   NOT NULL DEFAULT now()
);
CREATE INDEX students_branch_idx ON students (branch);

CREATE TABLE resumes (
    student_id   uuid PRIMARY KEY REFERENCES students (id) ON DELETE CASCADE,
    storage_key  text        NOT NULL,
    filename     text        NOT NULL,
    content_type text        NOT NULL,
    size_bytes   bigint      NOT NULL,
    uploaded_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE companies (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL UNIQUE REFERENCES users (id) ON DELETE CASCADE,
    name          text        NOT NULL,
    industry      text        NOT NULL DEFAULT '',
    hr_name       text        NOT NULL DEFAULT '',
    email         text        NOT NULL,
    phone         text        NOT NULL DEFAULT '',
    color         text        NOT NULL DEFAULT '#4f46e5',
    status        text        NOT NULL DEFAULT 'Pending' CHECK (status IN ('Pending', 'Approved', 'Rejected')),
    status_reason text        NOT NULL DEFAULT '',
    decided_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX companies_name_key ON companies (lower(name));

-- ---------------------------------------------------------------------------
-- Jobs and applications
-- ---------------------------------------------------------------------------
CREATE TABLE jobs (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id     uuid          NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    role           text          NOT NULL,
    type           text          NOT NULL CHECK (type IN ('Full-time', 'Internship')),
    location       text          NOT NULL,
    ctc            numeric(6, 2) NOT NULL CHECK (ctc >= 0),
    min_cgpa       numeric(4, 2) NOT NULL DEFAULT 0 CHECK (min_cgpa >= 0 AND min_cgpa <= 10),
    branches       text[]        NOT NULL CHECK (cardinality(branches) > 0),
    skills         text[]        NOT NULL DEFAULT '{}',
    deadline       date          NOT NULL,
    openings       integer       NOT NULL CHECK (openings > 0),
    description    text          NOT NULL,
    allow_backlogs boolean       NOT NULL DEFAULT false,
    status         text          NOT NULL DEFAULT 'Draft' CHECK (status IN ('Draft', 'Pending', 'Open', 'Closed', 'Rejected')),
    reject_reason  text          NOT NULL DEFAULT '',
    submitted_at   timestamptz,
    approved_at    timestamptz,
    closed_at      timestamptz,
    created_at     timestamptz   NOT NULL DEFAULT now(),
    updated_at     timestamptz   NOT NULL DEFAULT now()
);
CREATE INDEX jobs_company_idx ON jobs (company_id);
CREATE INDEX jobs_status_deadline_idx ON jobs (status, deadline);

CREATE TABLE applications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id     uuid        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    student_id uuid        NOT NULL REFERENCES students (id) ON DELETE CASCADE,
    stage      text        NOT NULL DEFAULT 'Applied'
        CHECK (stage IN ('Applied', 'Shortlisted', 'Interview', 'Offered', 'Rejected', 'Withdrawn')),
    note       text        NOT NULL DEFAULT '',
    applied_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (job_id, student_id)
);
CREATE INDEX applications_student_idx ON applications (student_id);
CREATE INDEX applications_job_stage_idx ON applications (job_id, stage);

CREATE TABLE application_events (
    id             bigserial PRIMARY KEY,
    application_id uuid        NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    from_stage     text,
    to_stage       text        NOT NULL,
    actor_user_id  uuid REFERENCES users (id) ON DELETE SET NULL,
    note           text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX application_events_app_idx ON application_events (application_id, id);

-- ---------------------------------------------------------------------------
-- Offers. A student is PLACED iff they hold an Accepted offer.
-- ---------------------------------------------------------------------------
CREATE TABLE offers (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid          NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    student_id     uuid          NOT NULL REFERENCES students (id) ON DELETE CASCADE,
    job_id         uuid          NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    company_id     uuid          NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    ctc            numeric(6, 2) NOT NULL CHECK (ctc >= 0),
    valid_until    timestamptz   NOT NULL,
    status         text          NOT NULL DEFAULT 'Pending'
        CHECK (status IN ('Pending', 'Accepted', 'Declined', 'Expired', 'Rescinded')),
    decline_reason text          NOT NULL DEFAULT '',
    created_at     timestamptz   NOT NULL DEFAULT now(),
    responded_at   timestamptz
);
-- At most one accepted offer per student, enforced by the database.
CREATE UNIQUE INDEX offers_one_accepted_per_student ON offers (student_id) WHERE status = 'Accepted';
CREATE UNIQUE INDEX offers_one_live_per_application ON offers (application_id) WHERE status IN ('Pending', 'Accepted');
CREATE INDEX offers_student_idx ON offers (student_id);
CREATE INDEX offers_company_idx ON offers (company_id);
CREATE INDEX offers_pending_valid_idx ON offers (valid_until) WHERE status = 'Pending';

-- ---------------------------------------------------------------------------
-- Drives, notices, site settings
-- ---------------------------------------------------------------------------
CREATE TABLE drives (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id       uuid          NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    job_id           uuid REFERENCES jobs (id) ON DELETE SET NULL,
    title            text          NOT NULL,
    starts_at        timestamptz   NOT NULL,
    duration_minutes integer       NOT NULL DEFAULT 180 CHECK (duration_minutes > 0),
    mode             text          NOT NULL CHECK (mode IN ('On-campus', 'Virtual', 'Off-campus')),
    venue            text          NOT NULL DEFAULT '',
    min_cgpa         numeric(4, 2) NOT NULL DEFAULT 0,
    branches         text[]        NOT NULL DEFAULT '{}', -- empty = every branch
    allow_backlogs   boolean       NOT NULL DEFAULT true,
    version          integer       NOT NULL DEFAULT 1,
    created_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now()
);
CREATE INDEX drives_starts_idx ON drives (starts_at);

CREATE TABLE drive_registrations (
    drive_id   uuid        NOT NULL REFERENCES drives (id) ON DELETE CASCADE,
    student_id uuid        NOT NULL REFERENCES students (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (drive_id, student_id)
);
CREATE INDEX drive_registrations_student_idx ON drive_registrations (student_id);

CREATE TABLE notices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title        text        NOT NULL,
    tag          text        NOT NULL DEFAULT 'General' CHECK (tag IN ('Result', 'Drive', 'Alert', 'Event', 'General')),
    published_at timestamptz NOT NULL DEFAULT now(),
    created_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notices_published_idx ON notices (published_at DESC);

CREATE TABLE site_settings (
    key        text PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Notifications: inbox + Web Push
-- ---------------------------------------------------------------------------
CREATE TABLE notifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type       text        NOT NULL,
    category   text        NOT NULL,
    title      text        NOT NULL,
    body       text        NOT NULL DEFAULT '',
    link       text        NOT NULL DEFAULT '',
    data       jsonb       NOT NULL DEFAULT '{}',
    dedupe_key text        NOT NULL,
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, dedupe_key)
);
CREATE INDEX notifications_user_created_idx ON notifications (user_id, created_at DESC, id);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;

CREATE TABLE notification_settings (
    user_id      uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    push_enabled boolean     NOT NULL DEFAULT true,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notification_preferences (
    user_id  uuid    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    category text    NOT NULL,
    push     boolean NOT NULL DEFAULT true,
    PRIMARY KEY (user_id, category)
);

CREATE TABLE push_subscriptions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    endpoint        text        NOT NULL UNIQUE,
    p256dh          text        NOT NULL,
    auth            text        NOT NULL,
    user_agent      text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_success_at timestamptz
);
CREATE INDEX push_subscriptions_user_idx ON push_subscriptions (user_id);

CREATE TABLE push_outbox (
    id              bigserial PRIMARY KEY,
    notification_id uuid        NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    subscription_id uuid        NOT NULL REFERENCES push_subscriptions (id) ON DELETE CASCADE,
    status          text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'sent', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until    timestamptz,
    last_error      text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz
);
CREATE INDEX push_outbox_due_idx ON push_outbox (next_attempt_at) WHERE status IN ('pending', 'processing');

-- ---------------------------------------------------------------------------
-- Audit log
-- ---------------------------------------------------------------------------
CREATE TABLE audit_log (
    id            bigserial PRIMARY KEY,
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    action        text        NOT NULL,
    entity        text        NOT NULL DEFAULT '',
    entity_id     text        NOT NULL DEFAULT '',
    meta          jsonb       NOT NULL DEFAULT '{}',
    ip            text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_created_idx ON audit_log (created_at DESC);

-- ---------------------------------------------------------------------------
-- Backstop: a placed student can never gain a new application or drive
-- registration, no matter which code path tries. Takes a share lock on the
-- student row so it serialises with offer acceptance (which locks FOR UPDATE).
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION forbid_placed_student() RETURNS trigger AS $$
BEGIN
    PERFORM 1 FROM students WHERE id = NEW.student_id FOR SHARE;
    IF EXISTS (SELECT 1 FROM offers WHERE student_id = NEW.student_id AND status = 'Accepted') THEN
        RAISE EXCEPTION 'student_placed' USING ERRCODE = 'PH001';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER applications_forbid_placed BEFORE INSERT ON applications
    FOR EACH ROW EXECUTE FUNCTION forbid_placed_student();
CREATE TRIGGER drive_registrations_forbid_placed BEFORE INSERT ON drive_registrations
    FOR EACH ROW EXECUTE FUNCTION forbid_placed_student();

-- +goose Down
DROP TRIGGER IF EXISTS drive_registrations_forbid_placed ON drive_registrations;
DROP TRIGGER IF EXISTS applications_forbid_placed ON applications;
DROP FUNCTION IF EXISTS forbid_placed_student();
DROP TABLE IF EXISTS audit_log, push_outbox, push_subscriptions, notification_preferences,
    notification_settings, notifications, site_settings, notices, drive_registrations, drives,
    offers, application_events, applications, jobs, companies, resumes, students,
    refresh_tokens, users;
