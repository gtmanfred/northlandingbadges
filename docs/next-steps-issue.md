# Ship North Landing badge service: remaining steps to production acceptance

The service in this repo is code-complete against [SPEC.md](../SPEC.md): full suite
passes with `-race` and no secrets, the deployable image builds (5.9 MB, distroless),
and a container smoke run confirms `/healthz` reports `EMAIL_MODE`, `/tasks/poll`
returns `401` without a token, and the fail-fast startup rules refuse bad config.

Everything left needs credentials, a real DiscGolfScene page, or a physical device —
none of which can be exercised from a local checkout. This issue tracks that work in
the order it has to happen.

---

## 1. Repository bootstrap

- [ ] `git init`, commit, push to GitHub (nothing is committed yet — the working tree
      is the only copy).
- [ ] Confirm default branch is `main` (`deploy.yml` and `ci.yml` both key off it).
- [ ] Enable GitHub Actions for the repo.
- [ ] Add branch protection on `main`: require the `ci` check, require PRs.
      *Acceptance: `ci.yml` runs on every PR and is a required status check.*
- [ ] Create the `production` environment (referenced by `deploy.yml`) and decide
      whether it needs a reviewer.

**Blocks everything else.** No workflow can run until this is done.

## 2. Gmail sender

- [ ] Create or designate the club Gmail account (spec §2 explicitly rules out custom
      domains, SPF, DKIM, MX).
- [ ] Enable 2FA, generate a **Google App Password** (not the account password).
- [ ] Note the deliverability trade-off: a plain `@gmail.com` sender with no SPF/DKIM
      alignment will land in spam for some recipients. This is accepted scope, but
      worth a line in the club's "check your junk folder" messaging.

## 3. Apple Wallet credentials

- [ ] Apple Developer portal → create Pass Type ID `pass.com.northlanding.badge`.
- [ ] Generate and download its certificate; export cert + private key as PEM.
- [ ] Download the Apple WWDR intermediate certificate as PEM.
- [ ] Record the 10-character Team ID.
- [ ] Set `APPLE_PASS_TYPE_ID`, `APPLE_TEAM_ID`, `APPLE_CERT_PEM`, `APPLE_KEY_PEM`,
      `APPLE_WWDR_PEM` as Fly secrets.
- [ ] **Verify on a real iPhone**: the emailed `.pkpass` opens, adds to Wallet, and
      shows the correct expiration.
      *Acceptance: Apple Wallet pass downloads on iOS and displays the correct dynamic
      expiration date.*

Signing is already implemented and unit-verified against throwaway test certs
(`internal/testkeys/`) — structure, manifest digests and PKCS#7 signature are covered.
Only the real trust chain is unverified.

## 4. Google Wallet credentials

- [ ] Create a Google Wallet issuer account; record the issuer ID.
- [ ] Create a **generic pass class**; record the full class ID
      (`<issuerId>.<classSuffix>`).
- [ ] Create a service account, grant it Wallet Object issuer access, download the
      JSON key.
- [ ] Set `GOOGLE_ISSUER_ID`, `GOOGLE_CLASS_ID`, `GOOGLE_SA_EMAIL`, and
      `GOOGLE_SA_KEY_PEM` (the `private_key` field from the JSON key).
- [ ] **Verify on a real device**: the "Save to Google Wallet" link opens and saves.
      *Acceptance: Google Wallet pass link correctly opens and saves to the user's
      Google account.*

Note: the JWT `origins` claim is currently left empty. If Google rejects saves from
the email link, populate it with the app origin — the field already exists in
`internal/wallet/googlepass`.

## 5. DiscGolfScene ingestion — decide Option A vs B

This is the largest open unknown in the spec, and the one most likely to need code.

- [ ] Log into the club manager backend and check whether a **webhook URL** can be
      configured (Option A, preferred).
  - [ ] If yes: point it at `POST /webhooks/discgolfscene`, set `DGS_WEBHOOK_SECRET`,
        and confirm the payload's actual field names. The parser accepts several
        aliases (`order_id` / `registration_id` / `id`, `item` / `pass_type` /
        `product`, `purchased_at` / `created_at` / `order_date`) — if the real payload
        uses none of them, add the alias in `internal/dgs/parse.go`.
  - [ ] Confirm what authentication header, if any, DiscGolfScene can send. The
        endpoint currently expects a bearer token; a signature-based scheme would need
        implementing.
- [ ] If webhooks are unavailable (Option B, the fallback):
  - [ ] Set `DGS_ROSTER_URL`, and `DGS_LOGIN_URL` / `DGS_USERNAME` / `DGS_PASSWORD`
        if the page needs a form login. **Verify the login flow**: the current
        implementation posts `username` / `password` form fields with a cookie jar. A
        CSRF token or multi-step login would need code.
  - [ ] Save the real (redacted) orders page over
        `internal/dgs/testdata/orders.html` so the fixture reflects production markup,
        then re-run `go test ./internal/dgs/`.
  - [ ] Confirm the parser finds the table: it locates columns by header name
        (aliases in `headerAliases`). Add aliases if the real headers differ.
  - [ ] Check whether the page paginates. Only the first page is fetched today; if
        orders paginate, pagination needs implementing or the hourly window relied on.
  - [ ] Confirm the real item labels classify. Anything that isn't recognisably a day
        pass or season membership is reported as unclassified and **not** mailed —
        by design, but it needs the real labels checked.
- [ ] Add `DGS_ROSTER_URL`, `DGS_LOGIN_URL`, `DGS_USERNAME`, `DGS_PASSWORD` as
      **repository secrets** for `contract-check.yml`.
      *Acceptance: daily contract-check workflow opens an issue when the parser stops
      extracting expected fields.*
- [ ] Run `contract-check.yml` manually once and confirm it passes against live data.

## 6. Fly.io deployment

- [ ] `fly apps create north-landing-badges` (or `fly launch --no-deploy`).
- [ ] `fly volumes create badges_data --region iad --size 1` — the dedupe ledger lives
      here; without it, a machine restart re-mails everyone.
- [ ] Set all secrets (see README "Deployment").
- [ ] Generate the poll token: `openssl rand -hex 32`. Set it as **both** the
      `POLL_TRIGGER_TOKEN` Fly secret and the GitHub Actions secret — the same value.
- [ ] Add the `FLY_API_TOKEN` repository secret.
- [ ] Set repository variables `APP_URL` and `EXPECTED_EMAIL_MODE`.
- [ ] First deploy; confirm it lands on `shared-cpu-1x` / 256MB.
      *Acceptance: deploys within a shared-cpu-1x (256MB/512MB) micro-instance.*
- [ ] Confirm the post-deploy smoke test passes.
      *Acceptance: post-deploy smoke test confirms the live app's reported EMAIL_MODE.*
- [ ] Verify a red CI run does not deploy (push a deliberately failing test on a
      branch, or inspect the `workflow_run` guard).
      *Acceptance: deploy workflow refuses to run when CI is red.*
- [ ] Confirm scale-to-zero actually happens and that a cold start completes inside
      the poll workflow's timeout (300s max, 30s connect, 3 attempts).

## 7. Acceptance testing against live data

Run in this order. Set `EXPECTED_EMAIL_MODE` to match each phase or the deploy smoke
test will (correctly) fail.

- [ ] **Phase 1 — `dry_run`.** `fly secrets set EMAIL_MODE=dry_run`, trigger
      `poll.yml` manually. Expect passes generated, emails logged, zero SMTP sends.
      *Acceptance: `EMAIL_MODE=dry_run` generates passes and logs rendered emails
      while sending zero SMTP messages.*
- [ ] **Phase 2 — `allowlist`.** `fly secrets set EMAIL_MODE=allowlist
      EMAIL_ALLOWLIST="<your address>"`, trigger a cycle against live registrations.
      Expect exactly one email to you; everyone else logged `skipped`.
      *Acceptance: only the tester is mailed; all other registrants are logged as
      skipped and no mail reaches them.*
- [ ] Inspect the received email on a phone and a desktop client: badge image renders,
      both wallet buttons work, expiration is correct.
- [ ] **Phase 3 — `live`.** `fly secrets unset EMAIL_MODE EMAIL_ALLOWLIST`; confirm
      `/healthz` reports `live`.
      *Acceptance: with `EMAIL_MODE` unset, the app sends normally.*
- [ ] Confirm dedupe across a real replay: trigger two cycles back to back, expect the
      second to report everything `already_seen` and send nothing.
      *Acceptance: no consumer receives duplicate badges.*
- [ ] Confirm the hourly schedule fires on its own and the run is visible in the
      Actions log.
      *Acceptance: hourly workflow triggers a poll cycle against the deployed app.*

**Important:** registrations processed during phases 1 and 2 are recorded as
processed. They will **not** be re-mailed when you switch to live — that is
deliberate (spec §4). If a real registrant was suppressed during testing and needs
their badge, clear that one row:

```sh
fly ssh console -C "sqlite3 /data/badges.db \
  \"DELETE FROM processed_registrations WHERE registration_id='DGS-88231'\""
```

Consider doing acceptance testing in a quiet period, or accept that early testers
need manual row clearing.

## 8. Operational readiness

- [ ] Write the club-facing note: what the email looks like, which sender it comes
      from, to check junk mail.
- [ ] Decide who watches `poll.yml` failures. The workflow fails when a cycle reports
      per-registration failures, so a red run means someone did not get a badge.
- [ ] Note the GitHub scheduling caveats already accepted in the spec: runs are
      best-effort and can be delayed several minutes, and **schedules are disabled
      after 60 days of repository inactivity**. Put a calendar reminder to push an
      empty commit if the repo goes quiet, or accept the risk.
- [ ] Decide on volume backup. A lost volume means the ledger resets and the next
      cycle re-mails every registration still on the roster page — the loudest
      possible failure. Options: periodic `fly ssh sftp get`, or accept it.
- [ ] Confirm the Apple pass download URL lifetime is acceptable: the emailed link
      carries a per-pass token and stays valid indefinitely. If that is not wanted,
      add expiry to the token check in `internal/server`.

## 9. Known gaps and follow-ups (not blocking)

- **No pass updates.** If a registrant's details change or a membership is refunded,
  nothing revokes or updates an issued pass. Apple's web service endpoints and
  Google's Objects API are both out of scope today.
- **No pagination on the roster scrape** (see §5).
- **`origins` unset on the Google Wallet JWT** (see §4).
- **Roster fetch reads only the first 8 MB** of the page (`maxPageBytes`) as a memory
  guard on a 256MB instance. Fine for a table; worth revisiting if the page grows.
- **`staticcheck` cannot run on a Go 1.24 toolchain locally** — the module requires
  1.25 (pulled in by `golang.org/x/net` and `golang.org/x/image`). CI installs 1.25,
  so `ci.yml` is unaffected.
- **Coverage is reported, not enforced.** Per spec §6, treat a drop in
  `internal/expiry`, `internal/mailer` or `internal/store` as a review blocker rather
  than adding a repo-wide threshold. Current: 100% / 93% / 77%, 86.5% overall.

---

## Acceptance criteria still unverified

Carried from SPEC.md §7 — everything not listed here has been verified locally.

- [ ] Deploys to Fly.io within a `shared-cpu-1x` micro-instance
- [ ] Hourly workflow triggers a cycle, visible in the Actions log
- [ ] Secure Gmail SMTP authentication established with a Google App Password
- [ ] Simulated webhook payload triggers a cleanly formatted email from the Gmail
      address
- [ ] `EMAIL_MODE=allowlist` mails only the tester against live data
- [ ] `EMAIL_MODE=dry_run` full cycle sends zero SMTP messages
- [ ] `EMAIL_MODE` unset sends normally
- [ ] Apple Wallet pass downloads on iOS with the correct expiration
- [ ] Google Wallet link opens and saves
- [ ] `ci.yml` is a required status check and a failing test blocks merge
- [ ] Deploy workflow refuses to run when CI is red
- [ ] Post-deploy smoke test confirms the live `EMAIL_MODE`
- [ ] Daily contract-check opens an issue when the parser breaks
