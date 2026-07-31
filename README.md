# North Landing Badge Distribution System

Digital badge delivery for [North Landing DGC](https://www.discgolfscene.com/club/7500/north-landing-dgc).
Physical badge handout at the on-site shop ends next year, so this service watches
DiscGolfScene registrations, generates a badge with the right expiration, and emails
it with Apple Wallet and Google Wallet passes attached.

Implements [SPEC.md](SPEC.md).

## How it works

```
GitHub Actions (hourly cron)
        │  POST /tasks/poll   (Bearer POLL_TRIGGER_TOKEN)
        ▼
   Fly.io machine (scales to zero between triggers)
        │
        ├── DiscGolfScene ── log in, POST CSV export ─→ parse ─→ classify pass type
        │                                                      │
        │                              SQLite claim (dedupe) ◄──┘
        │                                        │
        │                          expiration ─→ badge PNG ─→ .pkpass (PKCS#7)
        │                                                  └─→ Google Wallet JWT
        │                                        │
        │                            delivery guard (EMAIL_MODE)
        │                                        │
        └──────────────────── Gmail SMTP (app password) ─→ registrant
```

Each poll cycle logs in to DiscGolfScene, POSTs the club-admin CSV export for the
configured event, classifies each row's division into a badge tier (`MEM`,
`FNDR`, `SPON`), and derives each registration ID from the event slug plus the
registrant's email (case-insensitive). Rows without an email cannot be badged
and are reported in the cycle's `ingest_warnings` rather than aborting the run.

Alternative ingestion: `POST /webhooks/discgolfscene` runs the same pipeline for a
single registration if the club backend can post webhooks (spec §4, Option A).

### Expiration rules

| Pass | Expiration |
|---|---|
| Day Pass on date *X* | *X* + 1 day at 23:59:59, club-local |
| Season Membership (`MEM`) for season *YYYY* | Dec 31 *YYYY* at 23:59:59, club-local |
| Course Sponsor (`SPON`) for season *YYYY* | Dec 31 *YYYY* at 23:59:59, club-local |
| Course Founder (`FNDR`) | Never — issued once per registrant |

Season expiry comes from the season year of the DiscGolfScene event, **not** the
purchase date: registration for a season opens in November of the prior year, so
a purchase year would expire those badges before the season began. A season label
carrying no year is rejected rather than guessed at.

Founder badges never expire, so a founder who re-registers in a later season is
recorded as `skipped_founder_existing` and mailed nothing.

Calendar arithmetic runs in `CLUB_TIMEZONE` (default `America/New_York`), so a
Dec 31 day pass rolls into the next year, leap days are handled, and DST
transitions do not shift the wall clock.

## HTTP surface

| Route | Auth | Purpose |
|---|---|---|
| `GET /healthz` | none | Reports `status`, `email_mode`, `version`, schema version. Used by the post-deploy smoke test. |
| `POST /tasks/poll` | `Authorization: Bearer $POLL_TRIGGER_TOKEN` (or `X-Poll-Token`) | Runs one poll cycle. Idempotent. Returns a JSON report. `401` without a valid token. |
| `POST /webhooks/discgolfscene` | `DGS_WEBHOOK_SECRET` (falls back to the poll token) | Processes one registration payload. |
| `GET /passes/{id}.pkpass?t=…` | per-pass token from the email link | Serves the signed Apple Wallet bundle. |

There is no UI and no user database — DiscGolfScene remains the source of truth
(spec §2).

## Configuration

Required:

| Variable | Notes |
|---|---|
| `POLL_TRIGGER_TOKEN` | Shared secret with the `poll.yml` workflow. Startup fails without it. |
| `GMAIL_USER`, `GMAIL_APP_PASSWORD` | Google App Password, not the account password. Required unless `EMAIL_MODE=dry_run`. |

Optional:

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8080` | |
| `DB_PATH` | `/data/badges.db` | On the Fly volume. |
| `BASE_URL` | `http://localhost:8080` | Origin used to build the Apple Wallet download link. |
| `CLUB_TIMEZONE` | `America/New_York` | Drives every expiration wall clock. |
| `SMTP_ADDR` | `smtp.gmail.com:587` | |
| `EMAIL_FROM_NAME` | `North Landing DGC` | |
| `DGS_EVENT_SLUG` | — | Membership event slug, e.g. `North_Landing_Disc_Golf_Membership_2026_Season`. Update yearly. Unset means webhook-only; poll cycles become no-ops. |
| `DGS_SEASON_YEAR` | — | Season year, e.g. `2026`. Update yearly, alongside the slug. |
| `DGS_EMAIL`, `DGS_PASSWORD` | — | Club-admin login with staff access on that event. |
| `DGS_BASE_URL` | `https://www.discgolfscene.com` | Origin for login and the export. Override only for tests. |
| `DGS_WEBHOOK_SECRET` | poll token | Secret for the webhook endpoint. |
| `APPLE_PASS_TYPE_ID`, `APPLE_TEAM_ID`, `APPLE_CERT_PEM`, `APPLE_KEY_PEM`, `APPLE_WWDR_PEM`, `APPLE_ORG_NAME` | — | Unset means emails ship without a `.pkpass`. |
| `GOOGLE_ISSUER_ID`, `GOOGLE_CLASS_ID`, `GOOGLE_SA_EMAIL`, `GOOGLE_SA_KEY_PEM` | — | Unset means no Save-to-Google-Wallet button. |

### Delivery guard (acceptance testing)

Acceptance testing runs against live DiscGolfScene data, so outbound mail is
gated per recipient (spec §4):

| `EMAIL_MODE` | Behaviour | Recorded action |
|---|---|---|
| `live` (default when unset) | Mail goes to the real registrant. | `sent` |
| `allowlist` | Only `EMAIL_ALLOWLIST` addresses are mailed (case-insensitive, trimmed); everyone else is logged and skipped. | `sent` / `skipped` |
| `redirect` | Everything goes to `EMAIL_REDIRECT_TO`, subject prefixed `[would-send: real@user.com]`, body says so. | `redirected` |
| `dry_run` | Renders and logs the message, sends nothing. | `dry_run` |

The guard gates **only** the SMTP send. Badge generation, pass signing and the
SQLite dedupe record all run unchanged, so a guarded run exercises the whole
pipeline. A suppressed registration still counts as processed — re-sending after
switching to `live` requires deliberately clearing its dedupe row:

```sh
fly ssh console -C "sqlite3 /data/badges.db \"DELETE FROM processed_registrations WHERE registration_id='DGS-88231'\""
```

Startup fails fast on `allowlist` with an empty `EMAIL_ALLOWLIST`, or `redirect`
with no `EMAIL_REDIRECT_TO`.

## Local development

```sh
make test        # full suite with -race
make lint        # gofmt + go vet + staticcheck
make cover       # per-function coverage
make run         # run locally in dry_run mode
make golden      # regenerate golden HTML emails (review the diff)
```

No test sends real email: the SMTP capture server (`internal/smtptest`) binds to
`127.0.0.1` only, and no code path in the suite reaches `smtp.gmail.com`. The
suite passes with zero secrets configured.

Trigger a cycle locally:

```sh
curl -X POST localhost:8080/tasks/poll -H 'Authorization: Bearer local-dev-token'
```

Simulate a registration:

```sh
curl -X POST localhost:8080/webhooks/discgolfscene \
  -H 'Authorization: Bearer local-dev-token' \
  -d '{"order_id":"TEST-1","name":"Casey Chains","email":"you@example.com",
       "item":"Day Pass","purchased_at":"2026-07-04T10:15:00-04:00"}'
```

## Deployment

```sh
fly launch --no-deploy            # or: fly apps create north-landing-badges
fly volumes create badges_data --region iad --size 1

fly secrets set \
  POLL_TRIGGER_TOKEN="$(openssl rand -hex 32)" \
  GMAIL_USER="northlandingdgc@gmail.com" \
  GMAIL_APP_PASSWORD="xxxx xxxx xxxx xxxx" \
  DGS_EVENT_SLUG="North_Landing_Disc_Golf_Membership_2026_Season" \
  DGS_SEASON_YEAR="2026" \
  DGS_EMAIL="club-admin@example.com" DGS_PASSWORD="…" \
  APPLE_PASS_TYPE_ID="pass.com.northlanding.badge" \
  APPLE_TEAM_ID="ABCDE12345" \
  APPLE_CERT_PEM="$(cat apple-pass-cert.pem)" \
  APPLE_KEY_PEM="$(cat apple-pass-key.pem)" \
  APPLE_WWDR_PEM="$(cat wwdr.pem)" \
  GOOGLE_ISSUER_ID="33880000000…" \
  GOOGLE_CLASS_ID="33880000000….north-landing-badge" \
  GOOGLE_SA_EMAIL="wallet@project.iam.gserviceaccount.com" \
  GOOGLE_SA_KEY_PEM="$(cat google-sa-key.pem)"
```

GitHub Actions needs the repository secret `POLL_TRIGGER_TOKEN` (same value),
`FLY_API_TOKEN`, and for the daily contract check `DGS_EVENT_SLUG`,
`DGS_SEASON_YEAR`, `DGS_EMAIL`, `DGS_PASSWORD`. Optional repository variables:
`APP_URL`, `EXPECTED_EMAIL_MODE`.

Deploys happen from `deploy.yml` after `ci.yml` goes green on `main`, then a smoke
test asserts the live app's reported `EMAIL_MODE` matches `EXPECTED_EMAIL_MODE`.

### Acceptance testing against live data

```sh
fly secrets set EMAIL_MODE=allowlist EMAIL_ALLOWLIST="you@example.com"
# ... run poll.yml manually, confirm only you receive mail ...
fly secrets unset EMAIL_MODE EMAIL_ALLOWLIST   # back to live
```

Remember to set the `EXPECTED_EMAIL_MODE` repository variable while testing, or
the post-deploy smoke test will (correctly) fail the deploy.

### Wallet credentials

* **Apple** — in the Apple Developer portal create a Pass Type ID
  (`pass.com.northlanding.badge`), download its certificate, export the
  certificate and private key as PEM, and fetch the Apple WWDR intermediate.
  Passes are signed in-process with PKCS#7; no `openssl` binary is needed at
  runtime.
* **Google** — create a Google Wallet issuer account and a generic pass class,
  then a service account with Wallet Object issuer access. `GOOGLE_SA_KEY_PEM` is
  the `private_key` field from its JSON key.

Test keys in `internal/testkeys/` are self-signed throwaways so CI can verify
signing with no secrets. They are not Apple or Google credentials and no device
will trust them.

## Layout

```
cmd/server              service entrypoint and wiring
cmd/contract-check      daily live-parser check (opens an issue on failure)
internal/config         env loading + fail-fast validation
internal/domain         Registration, PassType, classification
internal/expiry         expiration arithmetic
internal/dgs            DiscGolfScene webhook parsing + CSV export ingest
internal/store          SQLite ledger, migrations, dedupe claims
internal/badge          badge PNG, .pkpass icon/logo artwork
internal/email          HTML/text rendering + MIME serialization
internal/mailer         delivery guard + SMTP transport
internal/wallet/applepass   signed .pkpass bundles
internal/wallet/googlepass  Save-to-Google-Wallet JWTs
internal/poll           the cycle that ties it together
internal/server         HTTP routes
internal/smtptest       in-process SMTP capture server (tests)
internal/integration    full-stack tests: real HTTP, real SQLite, real SMTP
```

## Deliberate trade-offs

* **SQLite on a volume, not Postgres.** The only state is "has this registration
  ID been handled". One file on the Fly volume costs nothing and keeps the app to
  a single container. A single writer connection removes `SQLITE_BUSY` handling
  entirely.
* **Pure-Go SQLite driver.** `modernc.org/sqlite` avoids cgo, so the image is a
  static binary on distroless.
* **Hand-rolled Google Wallet JWT and PKCS#7 via one small library.** No JWT
  dependency to keep current; signing is `crypto/rsa` plus `encoding/json`.
* **Claim-then-build dedupe.** The SQLite insert happens *before* the email, so
  concurrent or retried cycles cannot double-mail. If the pipeline fails before a
  delivery decision, the claim is released and the next cycle retries.
* **Guard-suppressed registrations stay processed.** Testing must not be able to
  double-mail real registrants later; recovering a specific badge is a deliberate
  manual delete.
* **The export is header-driven.** Columns are located by header name, not
  position, so a reordered export does not silently shift fields into the wrong
  ones. A row that cannot be parsed is reported and skipped rather than aborting
  the cycle.
* **No periodic Fly health check.** Polling `/healthz` would keep the machine
  awake and defeat scale-to-zero; the deploy smoke test hits it once instead.
