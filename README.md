# Placement Hub — backend

Go + Postgres API for the Placement Hub frontend: student, recruiter and placement-cell portals,
eligibility-aware jobs, applications and offers, campus drives, reports, and browser (Web Push) notifications.

The domain model mirrors `placementhub/lib/data.ts`; every export there has an endpoint here.
See [openapi.yaml](openapi.yaml) for the full API and [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) for the design decisions.

Live stack: **[Vercel](https://vercel.com)** (frontend) → **this API on [Render](https://render.com)** → **[Neon](https://neon.tech)** (Postgres).
Frontend repo: [placementhub-frontend](https://github.com/noobdivya/placementhub-frontend) — live at [placementhub-sepia.vercel.app](https://placementhub-sepia.vercel.app).
Live API: [placementhub-api-s9cj.onrender.com](https://placementhub-api-s9cj.onrender.com) (`/healthz`, `/readyz`).

## Quick start (local)

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

See [.env.example](.env.example) for the full list with comments. Summary:

| Variable | Required | Notes |
|---|---|---|
| `APP_ENV` | prod: `production` | enables production validation (`COOKIE_SECURE`, no dev `JWT_SECRET`) |
| `HTTP_ADDR` | no | default `:8080`; Render sets `PORT` — see below |
| `DATABASE_URL` | yes | `postgres://user:pass@host/db?sslmode=require` (Neon) |
| `JWT_SECRET` | yes | ≥ 32 random bytes — `openssl rand -base64 48` |
| `ACCESS_TOKEN_TTL`, `REFRESH_TOKEN_TTL` | no | defaults `15m`, `336h` |
| `FRONTEND_URL` | yes | the deployed frontend origin, e.g. `https://placementhub.vercel.app` |
| `CORS_ALLOWED_ORIGINS` | yes | comma-separated allow-list; must include every frontend origin that calls the API (prod + Vercel preview URLs if you use them) |
| `COOKIE_SECURE` | prod: `true` | must be `true` behind HTTPS |
| `COOKIE_SAMESITE` | cross-domain: `none` | **`none` on Render+Vercel** — frontend and backend are different domains, see below |
| `TRUSTED_PROXY` | prod: `true` | Render sits behind a proxy that sets `X-Forwarded-For` |
| `BOOTSTRAP_ADMIN_EMAIL/PASSWORD/NAME` | first deploy | creates the first admin account if none exists yet; safe to unset after |
| `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY` | no | generate with `go run ./cmd/vapid`; omit to disable push |
| `APP_TIMEZONE` | no | default `Asia/Kolkata` |
| `OFFER_VALIDITY_DAYS` | no | default `7` |
| `UPLOAD_DIR`, `MAX_UPLOAD_MB` | no | default `./uploads`, `5` — see disk note below |
| `RUN_WORKERS` | no | default `true`; set `false` on extra replicas so only one runs the push worker + scheduler |

## Deployment

### Overview

```
Neon (Postgres)  <──DATABASE_URL──  Render (this API, Docker)  <──NEXT_PUBLIC_API_URL──  Vercel (frontend)
```

Deploy in this order so each step has the value it needs from the previous one:

1. **Neon** — create the database, copy the connection string.
2. **Render** — deploy this repo with that connection string.
3. **[Vercel](https://github.com/noobdivya/placementhub-frontend#deployment)** — deploy the frontend pointed at the Render URL.
4. Come back to Render and set `CORS_ALLOWED_ORIGINS`/`FRONTEND_URL` to the final Vercel URL, then redeploy.

### 1. Database — Neon

1. Sign in at [neon.tech](https://neon.tech) and **New Project** (any region close to your Render region).
2. On the project dashboard, open **Connection Details** and copy the **pooled** connection string
   (host contains `-pooler`; the API uses a connection pool itself, and Render's free tier especially benefits from Neon's pooler).
3. It looks like:
   ```
   postgresql://<user>:<password>@ep-xxxx-pooler.<region>.aws.neon.tech/<dbname>?sslmode=require&channel_binding=require
   ```
   **Drop `&channel_binding=require`** — `pgx` (this API's driver) doesn't recognise that parameter and the connection
   will fail to establish. Keep `sslmode=require`. Also change the scheme from `postgresql://` to `postgres://`
   (both work with `psql`, but use `postgres://` here to match `.env.example`). Result:
   ```
   postgres://<user>:<password>@ep-xxxx-pooler.<region>.aws.neon.tech/<dbname>?sslmode=require
   ```
4. That's it — no manual migration step. The API runs `goose` migrations automatically against this URL on every startup (`internal/db.Migrate`, called from `cmd/api`).
5. Neon's free tier suspends an idle database and wakes it on the next connection (a few hundred ms of extra latency on the first request after idling) — expected, not an error.

### 2. API — Render

1. Sign in at [render.com](https://render.com) → **New +** → **Web Service** → connect the `placementhub-backend` GitHub repo.
2. **Language/Runtime: Docker.** Render detects the repo's `Dockerfile` automatically — leave build/start commands blank.
3. **Region:** pick one close to your Neon region. **Instance type:** the free tier works for evaluation; note its cold starts and ephemeral disk (below).
4. **Health check path:** `/healthz` (already implemented and used by the Dockerfile's own `HEALTHCHECK`).
5. **Environment variables** — add these under the service's *Environment* tab:

   | Key | Value |
   |---|---|
   | `APP_ENV` | `production` |
   | `DATABASE_URL` | the Neon connection string from step 1 |
   | `JWT_SECRET` | output of `openssl rand -base64 48` (or `[Generate]` if Render offers it) |
   | `COOKIE_SECURE` | `true` |
   | `COOKIE_SAMESITE` | `none` |
   | `TRUSTED_PROXY` | `true` |
   | `FRONTEND_URL` | `https://<your-project>.vercel.app` (placeholder is fine for the first deploy; fix after step 3 below) |
   | `CORS_ALLOWED_ORIGINS` | same as `FRONTEND_URL`, comma-separated if you add more origins later |
   | `BOOTSTRAP_ADMIN_EMAIL` | e.g. `admin@yourcollege.edu` |
   | `BOOTSTRAP_ADMIN_PASSWORD` | a temporary password, ≥ 10 chars — change it after first login |
   | `BOOTSTRAP_ADMIN_NAME` | e.g. `Placement Officer` |
   | `APP_TIMEZONE` | `Asia/Kolkata` (or your college's timezone) |

   Render sets `PORT` itself and the app already binds `:8080` inside the container, which Render's Docker
   runtime maps automatically — you don't need to set `HTTP_ADDR`.
6. **Web Push (optional):** run `go run ./cmd/vapid` locally, then add `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY`,
   and `VAPID_SUBJECT=mailto:you@yourcollege.edu` to the same Environment tab. Skip this and push notifications
   are simply off; the in-app inbox still works.
7. **Uploads persist across deploys only with a paid disk.** Render's free/starter web services have an ephemeral
   filesystem — anything written to `UPLOAD_DIR` (resumes) is lost on every redeploy or restart. For real use, either:
   - add a Render **Persistent Disk** mounted at `/data/uploads` (Render dashboard → the service → *Disks*), or
   - swap `internal/storage` for an S3-compatible backend (it's a small interface; see `internal/storage/storage.go`) — needed anyway to run more than one replica.
8. **Create Web Service.** Render builds the Docker image and deploys; watch the logs for `listening` (or check `https://<service>.onrender.com/healthz` → `200 ok`). Migrations run automatically before the server starts serving.
9. Note the assigned URL, e.g. `https://placementhub-backend.onrender.com` — the frontend needs it as `NEXT_PUBLIC_API_URL`.

**Why `COOKIE_SAMESITE=none`:** Vercel and Render are different domains, so the refresh-token cookie is
cross-site from the browser's point of view. Cross-site cookies require `SameSite=None; Secure` or the browser
drops them silently and refresh (and therefore staying logged in) breaks. `COOKIE_SECURE=true` is required
alongside it (the app already validates this combination at startup and refuses to boot otherwise).

### 3. Frontend — Vercel

See [placementhub-frontend/README.md](https://github.com/noobdivya/placementhub-frontend#deployment) — set `NEXT_PUBLIC_API_URL`
to the Render URL from step 2.9, deploy, then come back here and set `FRONTEND_URL` / `CORS_ALLOWED_ORIGINS`
to the exact `https://....vercel.app` domain Vercel assigns, and **manually redeploy** the Render service
(env var changes don't auto-restart running instances' validated config the same way a fresh deploy does —
use the *Manual Deploy* button, or just trigger it by pushing a commit).

### Verifying

```bash
curl https://<service>.onrender.com/healthz    # {"status":"ok"}  (liveness)
curl https://<service>.onrender.com/readyz     # checks the DB connection too
```

Then open the Vercel URL, log in with the `BOOTSTRAP_ADMIN_*` credentials, and change that password immediately
(`/change-password`) — it was set in a plaintext env var.

### Neon pooler gotchas (already fixed here, worth knowing if you touch `internal/db`)

Neon's pooled endpoint is PgBouncer in transaction-pooling mode, which is stricter about how a Postgres client
behaves than a direct connection. Two `pgx` defaults broke against it in production; both are already fixed in
this codebase, documented here so a future change to `internal/db` doesn't reintroduce them.

1. **Named prepared statements collide.** `pgx`'s default query mode caches a prepared statement per connection.
   A pooled connection can be handed to a different session between statements, so a cached statement name can
   collide with one an unrelated session already prepared on that backend — `FATAL: prepared statement
   "..." already exists` (SQLSTATE `08P01`), intermittently (a race, not deterministic). Fixed by setting
   `DefaultQueryExecMode = pgx.QueryExecModeExec` on both the pool (`db.Connect`) and the migration connection
   (`db.Migrate`) — unnamed statements via the extended protocol, so nothing to collide, with no real cost.
2. **That same mode change breaks plain `[]byte` JSON parameters.** Without the Describe step `QueryExecModeExec`
   skips, `pgx` has no way to learn a parameter's real column type, so it falls back to its default mapping for
   a bare `[]byte` — `bytea` — which Postgres then rejects when the target is `jsonb` (`invalid input syntax for
   type json`, SQLSTATE `22P02`). `encoding/json.RawMessage` (not a plain `[]byte`) has a default mapping
   straight to `json`/`jsonb` and needs no Describe. Every write to a jsonb column (`internal/audit.Log`,
   `internal/notify.Notifier.ToUsers`, `internal/site.Service.SetConfig`) passes `json.RawMessage`, not `[]byte`,
   for exactly this reason — keep it that way if you add another one.

### Generic Docker deploy (any host)

`docker build -t placementhub-api .` — a small non-root Alpine image with a health check (`/healthz`, `/readyz`). Run it behind HTTPS
(Web Push and secure cookies need it), set `APP_ENV=production`, `TRUSTED_PROXY=true` if a proxy sets `X-Forwarded-For`, and mount a volume at `/data/uploads`.
Uploads are on local disk behind the `storage.Storage` interface; swap in an S3-compatible implementation to run more than one replica.
