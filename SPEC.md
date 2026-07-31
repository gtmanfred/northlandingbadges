# Spec: North Landing Badge Distribution System

## 1. Context & Goal
North Landing DGC manages its club, season memberships, and day passes via [North Landing DGC](https://www.discgolfscene.com/club/7500/north-landing-dgc). Because physical badge distribution at the on-site shop will no longer be possible next year, this application automates digital badge delivery. 

The goal is to intercept registration events, generate a custom digital badge, and email it directly to the user with embedded Apple Wallet (`.pkpass`) and Google Wallet pass links reflecting their correct expiration details.

## 2. Scope Boundaries
### In-Scope
* Creating a lightweight webhooks/polling service hosted on Fly.io (utilizing the free/low-cost base tiers).
* An hourly GitHub Actions scheduled workflow that wakes the app and triggers a poll cycle.
* Parsing registration data from DiscGolfScene to detect "Day Pass" vs "Season Membership".
* Generating dynamic Apple Wallet and Google Wallet passes with automated expiration dates.
* Sending transactional emails from a generic `@gmail.com` address using secure SMTP/App Passwords.

### Explicit Non-Goals
* Building a custom front-end UI or user dashboard (all user interactions happen via email/wallet).
* Managing an independent user database (DiscGolfScene remains the sole source of truth).
* Handling direct financial transactions or payment processing.
* Setting up custom domain emails, SPF, DKIM, or MX records.

## 3. System Architecture & Tech Stack
To minimize costs on Fly.io, the application will run as a single, low-memory container.

* **Runtime:** Go (highly optimized for low-memory environments).
* **Database:** SQLite (embedded via Fly.io volumes) to track processed registration IDs and avoid duplicate emails.
* **Email Transport:** native net/smtp (Go) utilizing Google's SMTP server (`smtp.gmail.com`) authenticated via a secure Google App Password.
* **Wallet Engine:** native tools to sign Apple `.pkpass` files and generate Google Wallet JWT links.
* **Scheduler:** GitHub Actions (`schedule:` cron, hourly). No in-container cron daemon and no always-on process — the app may scale to zero and is woken by the inbound request.

## 4. Functional Requirements & Logic

### Data Ingestion (DiscGolfScene Integration)
> *Note: DiscGolfScene does not widely publish open webhook documentation. We must use one of two methods:*
* **Option A (Preferred if supported):** Configure a Webhook URL in the DiscGolfScene club manager backend.
* **Option B (Fallback):** Poll the DiscGolfScene club roster/orders page using credentials stored in environment variables.

#### Scheduled Trigger (GitHub Actions)
* A workflow in this repo runs on `schedule: cron: '0 * * * *'` (hourly, UTC) and issues an authenticated `POST` to the app's `/tasks/poll` endpoint.
* The workflow authenticates with a shared secret held in GitHub Actions secrets (`POLL_TRIGGER_TOKEN`) and matched against the same value in Fly.io secrets. Requests without a valid token get `401`.
* The workflow also supports `workflow_dispatch` for manual runs.
* The endpoint is idempotent: a poll cycle that finds no new registrations is a no-op, so duplicate or retried triggers are safe.
* Known limits accepted: GitHub's scheduled runs are best-effort and can be delayed by several minutes under load, and schedules are disabled automatically after 60 days of repo inactivity. Both are tolerable at hourly granularity.

### Business Rules & Expiration Calculation
* **Scenario A: Day Pass Purchased**
  * **GIVEN** a user purchases a Day Pass on `Date X`
  * **THEN** generate a digital badge where `Expiration Date = Date X + 1 Day (at 11:59 PM)`.
* **Scenario B: Season Membership Purchased**
  * **GIVEN** a user purchases a $50 Season Membership (division `MEM`) for season `YYYY`
  * **THEN** generate a digital badge where `Expiration Date = December 31, YYYY`, where `YYYY` is the season year of the event and not the year of purchase — season registration opens in November of the prior year.
* **Scenario C: Founder or Sponsor division**
  * **GIVEN** a season registration in division `FNDR`
  * **THEN** the badge never expires, and it is issued only on the registrant's first founder registration; later seasons record `skipped_founder_existing` and mail nothing.
  * **GIVEN** a season registration in division `SPON`
  * **THEN** the badge expires December 31 of the event's season year, with sponsor artwork and labelling.
  * **GIVEN** a season registration in an unrecognised division
  * **THEN** the row is rejected with `ErrUnknownPassType` and reported, never issued as a member badge.

### Email Delivery
* All emails must be dispatched from the designated Gmail address using secure app credentials stored in the Fly.io environment secrets (`GMAIL_USER`, `GMAIL_APP_PASSWORD`).
* The template must be highly scannable, featuring a clear image of the badge, a summary of their pass type, and two prominent buttons: **"Add to Apple Wallet"** and **"Save to Google Wallet"**.

#### Delivery Guard (Acceptance Testing)
During acceptance testing the app runs against live DiscGolfScene data, so outbound mail must be restricted to test recipients. Three environment variables control this, evaluated in order per recipient:

| Variable | Values | Behavior |
|---|---|---|
| `EMAIL_MODE` | `live` (default), `allowlist`, `redirect`, `dry_run` | Selects the delivery guard. |
| `EMAIL_ALLOWLIST` | Comma-separated addresses, e.g. `a@x.com,b@y.com` | In `allowlist` mode, only these recipients are mailed; all others are skipped and logged. |
| `EMAIL_REDIRECT_TO` | Single address | In `redirect` mode, every message is sent to this address instead of the real recipient. |

* `live` — normal behavior; mail goes to the real registrant.
* `allowlist` — send only if the registrant's address matches an `EMAIL_ALLOWLIST` entry (case-insensitive, whitespace-trimmed). Non-matching recipients are skipped.
* `redirect` — send every message to `EMAIL_REDIRECT_TO`, with the original recipient prefixed in the subject (`[would-send: real@user.com]`) and stated in the body header.
* `dry_run` — generate badge and passes, render the email, log it, send nothing.

Rules:
* The guard applies to the SMTP send only. Badge generation, pass signing, and SQLite dedupe recording all run unchanged, so a run under a guard exercises the full pipeline.
* A registration suppressed by the guard is still marked processed in SQLite. Re-sending after switching to `live` requires clearing its dedupe row — this is intentional so testing cannot double-mail real users later.
* Startup fails fast on inconsistent config: `allowlist` mode with an empty `EMAIL_ALLOWLIST`, or `redirect` mode with an unset `EMAIL_REDIRECT_TO`.
* Every guarded decision is logged with the registration ID, intended recipient, mode, and action taken (`sent`, `skipped`, `redirected`, `dry_run`).
* `EMAIL_MODE` defaults to `live` when unset, so a production deploy that omits these variables behaves normally.

## 5. Data Contracts & Wallet Schema
### Pass Fields Required:
* `passTypeIdentifier` / `Template ID`: North Landing Badge
* `serialNumber`: Unique DiscGolfScene Order/Registration ID
* `primaryFields`: Guest Name
* `secondaryFields`: Pass Type (Day Pass or Season Member)
* `expirationDate`: ISO 8601 Timestamp calculated at ingestion

## 6. Testing Strategy & CI

Development follows TDD: a failing test lands before the implementation that satisfies it. All tests run in GitHub Actions; a green CI run is the gate for merge and for deploy.

### Test Layers

**Unit** — pure logic, no I/O, no network. `go test ./...` with the race detector.
* Expiration calculation: Day Pass → `Date X + 1 day @ 23:59:59`; Season Membership → `Dec 31, YYYY`. Cover year-boundary purchases (Dec 31 day pass rolls into next year), leap day, and DST transitions in the club's local timezone.
* Pass-type classification from registration payloads, including unknown/malformed types (must error, not silently default).
* Delivery-guard decisions: table-driven over `EMAIL_MODE` × recipient, asserting `sent` / `skipped` / `redirected` / `dry_run` and the rewritten envelope in `redirect` mode.
* Config validation: the fail-fast startup rules from §4.

**Integration** — real SQLite file and real HTTP server, fakes only at the true external edges (SMTP, DiscGolfScene, Apple/Google signing).
* Dedupe: replaying the same registration ID twice sends exactly one email. Concurrent duplicate deliveries produce one send.
* `POST /tasks/poll` — valid `POLL_TRIGGER_TOKEN` runs a cycle; missing/wrong token returns `401` and does no work.
* Poll cycle end-to-end against a recorded DiscGolfScene fixture: ingest → classify → generate → guard → send, asserted against an in-process SMTP capture server rather than a mock, so the wire format is exercised.
* SQLite schema migrations apply cleanly to both an empty volume and a populated one.

**Artifact validation** — generated wallet passes checked structurally with test signing keys committed to the repo (never production certs).
* `.pkpass` is a valid zip, `manifest.json` hashes match every payload file, `signature` verifies against the test cert, and `pass.json` carries the required fields from §5.
* Google Wallet JWT parses, verifies against the test key, and its claims carry the expected expiration.
* Golden-file comparison of the rendered HTML email; a diff fails the build and requires the golden file be updated deliberately.

**Contract drift** — a separate scheduled job, not part of the merge gate.
* Because DiscGolfScene has no published API contract, a daily workflow fetches the live roster/orders page with test credentials and asserts the parser still extracts the expected fields. Failure opens an issue rather than failing a PR, since it detects an upstream change, not a regression in this repo.
* This job is the early-warning system for the scraping fallback (Option B) breaking silently.

### GitHub Actions Workflows

| Workflow | Trigger | Does |
|---|---|---|
| `ci.yml` | `pull_request`, `push` to default branch | `go vet`, `gofmt -l` check, `staticcheck`, `go test -race -coverprofile`, artifact validation. Required check for merge. |
| `deploy.yml` | `push` to default branch, after `ci.yml` succeeds | `flyctl deploy`, then a post-deploy smoke test. Never deploys on a red CI run. |
| `poll.yml` | `schedule` hourly, `workflow_dispatch` | Production poll trigger (§4). |
| `contract-check.yml` | `schedule` daily, `workflow_dispatch` | Live DiscGolfScene parser check; opens an issue on failure. |

### Rules
* `ci.yml` runs with no secrets available. Any test needing a credential is either a fake or is skipped with an explicit log line — a test must never pass by silently skipping.
* No test sends real email. The SMTP capture server binds to localhost; there is no code path in the test suite that can reach `smtp.gmail.com`.
* Post-deploy smoke test hits a `/healthz` endpoint and asserts the app reports its `EMAIL_MODE`, so a deploy accidentally left in `dry_run` or `allowlist` is visible immediately.
* Coverage is reported per run. Expiration logic, the delivery guard, and dedupe are the paths that must not regress; treat a coverage drop in those packages as a review blocker rather than enforcing a repo-wide percentage.

## 7. Acceptance Criteria
- [ ] Application successfully deploys to Fly.io within a `shared-cpu-1x` (256MB/512MB RAM) micro-instance.
- [ ] Hourly GitHub Actions workflow triggers a poll cycle against the deployed app and the run is visible in the Actions log.
- [ ] Poll endpoint rejects requests with a missing or wrong `POLL_TRIGGER_TOKEN` with `401`.
- [ ] Application safely stores incoming payloads in SQLite to ensure no consumer receives duplicate badges.
- [ ] Secure Gmail SMTP authentication is successfully established using a Google App Password.
- [ ] Testing a simulated webhook payload successfully triggers a cleanly formatted email from the Gmail address.
- [ ] With `EMAIL_MODE=allowlist` and `EMAIL_ALLOWLIST` set to a tester address, a live poll cycle mails only the tester; all other registrants are logged as `skipped` and no mail reaches them.
- [ ] With `EMAIL_MODE=dry_run`, a full poll cycle generates passes and logs rendered emails while sending zero SMTP messages.
- [ ] With `EMAIL_MODE` unset, the app sends normally (defaults to `live`).
- [ ] App refuses to start in `allowlist` mode with an empty `EMAIL_ALLOWLIST`, or `redirect` mode with no `EMAIL_REDIRECT_TO`.
- [ ] Apple Wallet pass successfully downloads on iOS and displays the correct dynamic expiration date.
- [ ] Google Wallet pass link correctly opens and saves to the user's Google account.
- [ ] `ci.yml` runs on every pull request and is a required status check; a failing test blocks merge.
- [ ] Full test suite passes with `-race` and completes without any secret configured.
- [ ] Deploy workflow refuses to run when CI is red.
- [ ] Post-deploy smoke test confirms the live app's reported `EMAIL_MODE`.
- [ ] Daily contract-check workflow opens a GitHub issue when the DiscGolfScene parser stops extracting expected fields.
