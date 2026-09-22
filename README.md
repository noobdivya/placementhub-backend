# Placement Hub — backend

Go + Postgres API for the Placement Hub frontend: student, recruiter and placement-cell portals,
eligibility-aware jobs, applications and offers, campus drives, reports, and browser (Web Push) notifications.

The domain model mirrors `placementhub/lib/data.ts`; every export there has an endpoint here.
See [openapi.yaml](openapi.yaml) for the full API and [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) for the design decisions.

## Quick start

```bash
docker compose up -d db          # Postgres on localhost:5433
cp .env.example .env             # optional; sensible dev defaults exist
go run ./cmd/seed                # sample data mirroring the frontend (empty DB only)
go run ./cmd/api                 # http://localhost:8080  (migrations run at startup)
```

Sample logins after seeding: `admin@college.edu` / `Admin@12345`,
`careers@nimbuslabs.example` / `Company@12345`, `ananya.sharma@college.edu` / `Student@12345`.

Everything in one go: `docker compose up --build` starts the database and the API container.

**Web Push:** `go run ./cmd/vapid` prints a key pair; put it in `.env` (`VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY`).
Without keys the API runs normally and push is simply disabled (the in-app inbox still works).

## Tests

```bash
docker compose up -d db
REQUIRE_DB=1 go test ./... -count=1
```

Integration tests give every test its own freshly migrated throwaway database on the compose Postgres
(`TEST_DATABASE_URL` overrides the server; without a reachable Postgres, DB tests skip unless `REQUIRE_DB=1`).
The suite also passes with the race detector (`go test -race`; on a machine without a C compiler run it in the Go Docker image).

## Business rules

| Rule | Where it is enforced |
|---|---|
| **A student who accepts an offer is PLACED.** They can no longer apply to jobs or register for drives, but everything stays visible in their history. | Services (`application`, `drive`) **and** a database trigger (`forbid_placed_student`) **and** a unique index (`offers_one_accepted_per_student`) |
| Accepting an offer withdraws the student's other live applications, declines their other pending offers, and cancels their registrations for drives that have not started. Applications are kept, stage `Withdrawn`. | `application.Service.Accept`, one transaction under a lock on the student row |
| Eligibility = `cgpa >= min` AND branch allowed AND (no backlogs OR job allows them) AND not placed. | One SQL definition in `internal/eligibility`, shared by the jobs list, apply check, drive counts and notification fan-out |
| Applying needs: open job, before the deadline day ends (college timezone), eligible, resume uploaded, not already applied. | `application.Service.Apply` |
| Stages: Applied → Shortlisted → Interview → Offered, reject from any live stage, reset to Applied. No skipping. | `application.CanTransition` |
| Reaching **Offered** creates an offer (default CTC = the job's, valid 7 days). Leaving Offered rescinds an unanswered offer. Unanswered offers expire automatically. | `application`, scheduler |
| Only **Approved** companies can submit jobs; jobs go live only after placement-cell approval. Once live, eligibility criteria are frozen (deadline/openings/description can still change). | `job.Service` |
| Students cannot edit CGPA, branch, backlogs, roll, name or email; only the placement cell can. | `student.ProfileUpdate` vs `student.AdminUpdate` |
| The placement cell can revoke a placement (e.g. the company withdrew): the student becomes unplaced and may apply again. | `POST /admin/offers/{id}/revoke` |

Student status in every screen is derived, never stored: **Placed** (holds an accepted offer) → **In process** (a live application) → **Unplaced**.

## Notifications (V1)

Students only (companies and the placement cell get no push in V1; the schema is keyed by `user_id`, so adding them later needs no migration).

| Event | Recipients | Push category |
|---|---|---|
| New job approved | **Eligible, unplaced students only** | `new_job` (mutable) |
| Shortlisted / rejected | the applicant | `application_update` (mutable) |
| Interview scheduled, offer received, offer expiring/expired, placement confirmed/revoked | the student | `critical` (not mutable per category) |
| Deadline in 48h / 24h | eligible students who have not applied | `deadline` (mutable) |
| Drive announced / rescheduled / cancelled / tomorrow | eligible / registered students | `drive` (mutable) |
| Notice with `notify: true` | all students, or chosen branches | `notice` (mutable) |

- Every notification is written to the **inbox** (`/me/notifications`) inside the same transaction as the change that caused it, deduplicated by a per-event key.
- **Muting** a category stops the *push* only; the item still appears silently in the inbox. Interviews and offers ignore category mutes; only the **master switch** (`pushEnabled: false`) silences everything, and each device can be removed (`DELETE /me/push-subscriptions`).
- Delivery: a worker drains `push_outbox` (`FOR UPDATE SKIP LOCKED`), retries with exponential backoff (30s → 1h, 6 attempts), deletes subscriptions the push service reports gone (404/410), and skips a push if the student already read the notification.
- Push endpoints must belong to a known browser push service (FCM, Mozilla, Apple, Windows) and the sender never follows redirects — the server would otherwise be an SSRF proxy.
- The scheduler (deadline/offer/drive reminders, closing expired jobs, cleanup) ticks every minute; a Postgres advisory lock lets several API replicas run safely.

Frontend needs: `public/sw.js` handling `push` (`{title, body, url, tag}`) and `notificationclick`, a permission prompt triggered by a button click, and `GET /push/vapid-public-key`. iOS Safari only supports Web Push for sites added to the Home Screen.

## Security

- **Auth:** bcrypt passwords; 15-minute JWT access tokens (HS256, algorithm/issuer/expiry enforced); rotating refresh tokens in an `httpOnly`, `SameSite` cookie scoped to `/auth`, stored only as SHA-256 hashes. Reusing a rotated refresh token revokes the whole session family. Login answers identically for unknown email and wrong password, spends equal time, and is rate limited per IP and per account.
- **Accounts:** students are created by the placement cell (single or CSV import) with a one-time temporary password that must be changed before anything else works. Companies self-register as *Pending*. Admins add admins. There is no email channel, so password resets are admin-issued temporary passwords.
- **Access control:** role checked in middleware, ownership checked in services (a company only ever sees its own jobs/candidates; a student only their own data). Other people's resources answer `404`, not `403`.
- **Input:** JSON bodies capped at 1 MiB and validated field-by-field; uploads must be real PDFs (magic bytes + content sniffing, ≤ 5 MB), stored under generated keys and served as `attachment` with `nosniff`; all SQL is parameterised; `LIKE` wildcards in search are escaped; CSV exports neutralise spreadsheet formulas.
- **HTTP:** strict security headers, CORS allow-list with credentials, `Origin` check on cookie-authenticated endpoints, request ids, panic recovery, generic 500s that never leak internals. In production the server refuses to start without a strong `JWT_SECRET` and `COOKIE_SECURE=true`.
- **Audit log** for logins, password changes, approvals, offer revocations, imports, admin creation, etc.

Known trade-offs: an access token stays valid until it expires (≤ 15 min) after an account is deactivated (its refresh tokens are revoked immediately); using the app in two tabs that refresh at the same instant can trip refresh-reuse detection and sign the user out.

## Layout

```
cmd/api        server (migrations, workers, graceful shutdown)   cmd/seed   sample data   cmd/vapid   key generator
db/migrations  goose SQL migrations (embedded)
internal/
  app          wiring + routes        auth  passwords, JWT, refresh, RBAC       eligibility  the one eligibility rule
  student company job application drive notice site report      (handler.go + service.go per feature)
  notify       inbox, preferences, subscriptions, fan-out       push  worker + Web Push sender
  scheduler    periodic jobs          storage  local-disk uploads      httpx  errors, validation, middleware
  testutil     isolated DB per test, fake clock/push, fixtures
```

Queries are plain `pgx` with SQL in the services (the plan mentioned `sqlc`; hand-written SQL kept the toolchain to `go build`). Lists such as skills and eligible branches are `text[]` columns rather than join tables.

## Configuration

See [.env.example](.env.example). Key variables: `DATABASE_URL`, `JWT_SECRET`, `CORS_ALLOWED_ORIGINS`, `COOKIE_SECURE`,
`VAPID_*`, `BOOTSTRAP_ADMIN_*` (creates the first admin when none exists), `APP_TIMEZONE`, `OFFER_VALIDITY_DAYS`, `UPLOAD_DIR`.

## Deploying

`docker build -t placementhub-api .` — a small non-root Alpine image with a health check (`/healthz`, `/readyz`). Run it behind HTTPS
(Web Push and secure cookies need it), set `APP_ENV=production`, `TRUSTED_PROXY=true` if a proxy sets `X-Forwarded-For`, and mount a volume at `/data/uploads`.
Uploads are on local disk behind the `storage.Storage` interface; swap in an S3-compatible implementation to run more than one replica.
