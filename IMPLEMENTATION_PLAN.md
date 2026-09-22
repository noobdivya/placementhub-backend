# Placement Hub Backend — Implementation Plan (V1)

> **Status: all phases (0–7) are implemented and tested.** See [README.md](README.md) for how to run it and
> [openapi.yaml](openapi.yaml) for the API. This document keeps the original design; the deviations are listed here.
>
> **Added after the original plan**
> - **Placement rule:** accepting an offer makes the student PLACED. They can no longer apply to jobs or register for drives
>   (history stays visible). New `offers` lifecycle (`Pending → Accepted | Declined | Expired | Rescinded`), a `Withdrawn`
>   application stage, and a DB trigger + unique index as backstops. Admin can revoke a placement.
> - Admin-issued temporary passwords (students, recruiters, new admins) with a forced change at first login, instead of an email flow.
> - CSV import/export, audit log, application withdrawal, candidate detail + resume download for recruiters.
>
> **Deviations from the plan below**
> - Plain `pgx` + SQL in the services instead of `sqlc`; `text[]` columns instead of `job_branches` / `student_skills` / `job_skills` tables.
> - Hand-rolled validation instead of `go-playground/validator`; tests use a throwaway database per test on the compose Postgres
>   instead of `testcontainers-go`.
> - `notification_preferences` stores only `push` (muting never hides the inbox entry), plus a master switch in `notification_settings`.
> - Students cannot edit their own CGPA/branch/backlogs (the placement cell does); the profile page's CGPA field should be read-only.
> - Not built (not needed for V1): SSE stream for the inbox (it polls), S3 storage (local disk behind an interface).

Source of truth for the domain: `placementhub/lib/data.ts`. Each export there becomes an API endpoint.

## Scope

- Roles: **student**, **company**, **admin** (placement cell), plus a public home page.
- Notifications V1: **browser (Web Push) notifications for eligible students only**, with mute controls. No email.
- Companies and the placement cell get no push in V1. The schema is keyed by `user_id`, so adding them later needs no migration, only new triggers and a bell in their layouts.

## Stack

| Concern | Choice |
|---|---|
| Language / router | Go 1.27, `chi` + `net/http` |
| Database | Postgres (docker compose), `pgx` + `sqlc`, `goose` migrations |
| Auth | Email + password (bcrypt), short-lived JWT access token, refresh token in httpOnly cookie |
| Push | `github.com/SherClockHolmes/webpush-go`, VAPID keys via env |
| Files | `Storage` interface; local disk first, S3-compatible later |
| Other | `slog`, `go-playground/validator`, `testcontainers-go`, hand-written `openapi.yaml` |

## Layout

```
placementhub-backend/
  cmd/api/main.go            # replaces the empty server.go
  internal/
    config/ httpx/ auth/     # env config, JSON + errors + middleware, JWT + RBAC
    student/ company/ job/ application/ drive/ notice/ report/
    eligibility/             # single SQL predicate, shared everywhere
    notify/                  # Notifier interface, inbox, push worker
    storage/                 # resume upload
  db/migrations/ db/queries/
  docker-compose.yml  Makefile  openapi.yaml
```

Each domain package: `handler.go`, `service.go`, `repo.go`. Business rules live in the service.

## Data model

- `users` (email, password_hash, role)
- `students` (roll, branch, year, cgpa, backlogs, tenth, twelfth, phone, links, about, resume) and `student_skills`
- `companies` (name, industry, hr, color, status: Pending | Approved | Rejected)
- `jobs` (company, role, type, location, ctc, min_cgpa, openings, deadline, description, allow_backlogs, status: Draft | Pending | Open | Closed), `job_branches`, `job_skills`
- `applications` (UNIQUE job_id + student_id, stage, note) and `application_events` (stage history)
- `offers` (application, ctc, date). Source of truth for "Placed".
- `drives`, `drive_registrations`, `notices`, `site_settings`
- Notifications:
  - `notifications` (user_id, type, title, body, link, data jsonb, dedupe_key, read_at, created_at, UNIQUE user_id + dedupe_key)
  - `notification_preferences` (user_id, type, push bool, in_app bool)
  - `push_subscriptions` (user_id, endpoint UNIQUE, p256dh, auth, user_agent, last_success_at)
  - `push_outbox` (notification_id, subscription_id, status, attempts, next_attempt_at)

Derived, never stored: `Job.applicants`, `Company.openRoles` / `hires`, `Drive.registered` / `eligible`, `Student.status` (offer → Placed, active application → In process, else Unplaced), job and application `color` (from company).

## Business rules (enforced server-side)

- **Eligibility:** `cgpa >= min_cgpa` AND branch in `job_branches` AND (backlogs = 0 OR `allow_backlogs`). One SQL predicate in `internal/eligibility`, used by the "eligible only" filter, the apply check, `Drive.eligible`, and notification fan-out.
- **Apply:** job is Open, before its deadline, not already applied, student is eligible and has a resume. Otherwise 422 with the reason.
- **Stage moves:** Applied → Shortlisted → Interview → Offered. Reject from any stage. Reset to Applied allowed. Every move writes an `application_events` row. Moving to Offered creates the `offers` row.
- **Approval:** only Approved companies can submit jobs. Submitted jobs are Pending until an admin approves, then they go Open.
- **Closing:** jobs close after the deadline (lazy check on read plus a scheduled sweep).
- **Ownership:** a company touches only its own jobs. A student sees only their own applications.

## API

```
POST /auth/login | /auth/refresh | /auth/logout | /auth/register/company
GET  /me

# student
GET  /jobs?type=&q=&eligible=&sort=        POST /jobs/{id}/apply
GET  /me/applications                      GET|PUT /me/profile
POST /me/resume   GET /me/resume           POST|DELETE /me/skills

# company
GET|POST /company/jobs    PUT /company/jobs/{id}    POST /company/jobs/{id}/submit|close
GET  /company/candidates?jobId=            PATCH /company/applications/{id}/stage

# placement cell
GET  /admin/students?q=&branch=&status=    GET /admin/companies?status=
PATCH /admin/companies/{id}/status         PATCH /admin/jobs/{id}/approve|reject
CRUD /admin/drives                         POST /admin/students/import (CSV)
GET  /admin/reports/summary|monthly-offers|branches|ctc-bands|top-recruiters|recent-offers

# notifications (student)
GET  /me/notifications?unread=&cursor=     GET /me/notifications/unread-count
POST /me/notifications/{id}/read           POST /me/notifications/read-all
GET|PUT /me/notification-preferences
GET  /push/vapid-public-key
POST /me/push-subscriptions                DELETE /me/push-subscriptions   # body: {endpoint}

# public
GET  /notices   GET /recruiters   GET /site-config
```

Errors use one shape: `{error:{code,message,fields}}`. Lists paginate. Role checks are in middleware, ownership checks in services.

## Notifications (V1)

**Recipients: students only.**

| Event | Recipients | Fires |
|---|---|---|
| New job approved | Eligible students only | On admin approval |
| Application stage change (incl. interview note) | The applicant | Inside the stage-move transaction |
| Deadline in 48h / 24h | Eligible students who haven't applied | Scheduled worker (5c) |
| Offer about to expire | The student with the offer | Scheduled worker (5c) |
| Drive announced / rescheduled / upcoming | Registered or eligible students | On drive change + scheduler (5c) |

**New-job flow.** On approval, one transaction selects eligible students via the shared predicate, inserts one `notifications` row each (dedupe key `job_new:{jobId}`), and queues `push_outbox` rows only for students whose `new_job` preference has push on. A worker goroutine in the same binary claims rows with `FOR UPDATE SKIP LOCKED`, sends via Web Push, and retries with backoff. `410 Gone` / `404` deletes the dead subscription. Ineligible students never get a row or a push.

**Mute controls.**
- Per device: "Disable on this device" deletes that browser's subscription.
- Per category: New jobs, Application updates, Deadlines, Drives. Turns push off across all devices.
- Muting stops the push but the item still appears silently in the in-app inbox.
- Offers and interview schedules cannot be muted.

**Frontend.**
- `public/sw.js` handles `push` and `notificationclick` (opens the notification's `link`).
- The permission prompt comes only from a button click (bell dropdown and profile settings), never on page load.
- The bell, unread count and `/students/notifications` inbox are the reliable record, since push delivery is not guaranteed.
- The inbox polls every 30–60s in V1.

**Constraints.**
- HTTPS is required in production (`localhost` works in dev).
- iOS Safari (16.4+) supports Web Push only for a site added to the Home Screen. `site.webmanifest` and icons already exist, so students just need an "Add to Home Screen" hint.
- Payloads are small: `{title, body, url, tag}`.

## Phases

| # | Milestone | Done when |
|---|---|---|
| 0 | **Foundation**: module, compose Postgres, config, chi server, `/healthz`, migrations, error + logging middleware, CI. `Notifier` interface created here so later features call it from the start. | `make up && curl /healthz` works |
| 1 | **Auth and roles**: users, login, refresh, RBAC middleware, seed script loading the dummy data from `data.ts` | Three seeded logins work, role guards tested |
| 2 | **Students**: profile CRUD, skills, resume upload/download | Profile page runs on real data |
| 3 | **Companies and jobs**: registration, admin approval, job CRUD with draft → submit → approve, eligible listing, `eligibility` package | Jobs board and post-job form run on real data |
| 4 | **Applications and pipeline**: apply with eligibility checks, my applications, candidate board, stage transitions, offers | Apply and kanban work end to end |
| 5a | **Notifications inbox**: notifications table, inbox endpoints, stage-change + new-job triggers | Bell shows real notifications |
| 5b | **Web Push**: subscriptions, VAPID, push worker, service worker, mute preferences | Eligible students get a browser notification for a new job, ineligible do not, mute works |
| 5c | **Scheduler + drives**: deadline and offer-expiry reminders, drive CRUD, registration, drive triggers, notices | Drives page and reminders live |
| 6 | **Reports**: aggregate queries for all dashboard charts, CSV export | Reports page matches seed numbers |
| 7 | **Hardening**: auth rate limiting, CORS for the Next.js origin, upload validation, audit logging, OpenAPI, Dockerfile, deploy target | Integration tests pass in CI, container runs |

The frontend replaces one slice of `lib/data.ts` with a typed API client per phase.

## Frontend contract gaps to fix

- `PostJobForm` has an "allow backlogs" checkbox, but `Job` has no field for it. Add `allowBacklogs`.
- The form says "Submit for approval", but `JobStatus` is `Open | Closed | Draft`. Add `Pending`.
- IDs like `"j1"` become UUID strings (still strings).
- Placeholder values such as `lastVisit: "—"` become `null`.
- The resume endpoint must return upload date and size (the profile page shows both).

## Testing

- Unit tests for the eligibility predicate and stage-transition rules.
- Integration tests with `testcontainers-go` for apply, approval, stage moves and notification fan-out.
- A specific test that a new-job approval notifies exactly the eligible students and nobody else, and that muted students get an inbox row but no push.

## Open decisions (defaults assumed until you say otherwise)

1. **Database:** Postgres.
2. **Login:** email + password. Students are bulk-imported from CSV by the placement cell.
3. **Placed students:** allowed to keep applying (no blocking policy) in V1.
4. **Resume storage:** local disk behind the `Storage` interface.
5. **Repo layout:** `git init` the backend as its own repo, separate from the frontend.
