// Package integration_test wires the real components together — real SQLite file,
// real HTTP server, real SMTP conversation — with fakes only at the true external
// edges: DiscGolfScene (a recorded fixture served over HTTP) and the wallet
// signing keys (throwaway test certs). Spec §6, integration layer.
package integration_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/dgs"
	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/mailer"
	"github.com/northlanding/badges/internal/poll"
	"github.com/northlanding/badges/internal/server"
	"github.com/northlanding/badges/internal/smtptest"
	"github.com/northlanding/badges/internal/store"
	"github.com/northlanding/badges/internal/testkeys"
	"github.com/northlanding/badges/internal/wallet/applepass"
	"github.com/northlanding/badges/internal/wallet/googlepass"
)

const pollToken = "poll-trigger-token"

// The membership export identifies an event by slug, not by URL path, so the
// fake server and the client config must agree on these out of band.
const (
	dgsEventSlug  = "Slug"
	dgsSeasonYear = 2026
	dgsEmail      = "club-admin@example.com"
	dgsPassword   = "dgs-admin-password"
)

// membershipExportCSV stands in for the club-admin CSV export. It carries one
// row per tier the pipeline needs to cover (MEM, SPON, FNDR) plus a division
// the club has never used, which must surface as an ingest warning rather than
// a badge — the export has no day passes, unlike the retired HTML roster page.
const membershipExportCSV = "Division,Name,Email,Registration date EST\n" +
	"MEM,Casey Chains,casey@example.com,2026-07-04 10:15:00\n" +
	"SPON,Robin Rollaway,robin@example.com,2026-04-01 08:02:00\n" +
	"FNDR,Dana Discraft,dana@example.com,2026-12-31 21:40:00\n" +
	"PRO,Jamie Jomez,jamie@example.com,2026-06-12 17:30:00\n" +
	"Totals,,,\n"

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// idFor derives the registration ID the export client computes for an email,
// so tests can look up store records without hard-coding a hash.
func idFor(email string) string { return dgs.RegistrationID(dgsEventSlug, email) }

// newFakeDGS emulates DiscGolfScene's authenticated CSV export closely enough
// for the pipeline to exercise its real login-then-export flow: a sign-in form
// that sets a session cookie, and an admin export endpoint that demands it.
func newFakeDGS(t *testing.T, hits *atomic.Int32) config.DGSConfig {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/sign-in", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.PostFormValue("auth_email") != dgsEmail || r.PostFormValue("auth_password") != dgsPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "dgs_session", Value: "ok", Path: "/"})
	})
	mux.HandleFunc("/tournaments/"+dgsEventSlug+"/admin/registration-export", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if _, err := r.Cookie("dgs_session"); err != nil {
			// An unauthenticated export answers with the sign-in page, per the
			// real DiscGolfScene behaviour the client's re-login retry expects.
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><form class="form-signin"></form></html>`))
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte(membershipExportCSV))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return config.DGSConfig{
		BaseURL: srv.URL, EventSlug: dgsEventSlug, SeasonYear: dgsSeasonYear,
		Email: dgsEmail, Password: dgsPassword,
	}
}

type stack struct {
	client   *http.Client
	pollSvc  *poll.Service
	baseURL  string
	smtp     *smtptest.Server
	store    *store.Store
	dgsHits  *atomic.Int32
	dbPath   string
	location *time.Location
}

// newStack assembles the full app the way cmd/server does.
func newStack(t *testing.T, mode config.EmailMode, allowlist []string, redirectTo string) *stack {
	t.Helper()

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	var hits atomic.Int32
	dgsConfig := newFakeDGS(t, &hits)

	smtpServer, err := smtptest.Start(smtptest.Options{
		RequireAuth: true, Username: "club@gmail.com", Password: "app-password",
	})
	if err != nil {
		t.Fatalf("smtptest.Start: %v", err)
	}
	t.Cleanup(func() { _ = smtpServer.Close() })

	dbPath := filepath.Join(t.TempDir(), "badges.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := &config.Config{
		DBPath:           dbPath,
		PollTriggerToken: pollToken,
		WebhookSecret:    "webhook-secret",
		EmailMode:        mode,
		Allowlist:        allowlist,
		RedirectTo:       redirectTo,
		GmailUser:        "club@gmail.com",
		GmailAppPassword: "app-password",
		SMTPAddr:         smtpServer.Addr(),
		FromName:         "North Landing DGC",
		ClubTimezone:     ny,
		DGS:              dgsConfig,
		Apple: config.AppleConfig{
			PassTypeIdentifier: "pass.com.northlanding.badge",
			TeamIdentifier:     "TESTTEAM01",
			OrganizationName:   "North Landing DGC",
			CertPEM:            testkeys.ApplePassCertPEM(),
			KeyPEM:             testkeys.ApplePassKeyPEM(),
			WWDRPEM:            testkeys.AppleWWDRPEM(),
		},
		Google: config.GoogleConfig{
			IssuerID:            "3388000000012345678",
			ClassID:             "3388000000012345678.north-landing-badge",
			ServiceAccountEmail: "wallet@north-landing.iam.gserviceaccount.com",
			KeyPEM:              testkeys.GoogleServiceAccountKeyPEM(),
		},
	}

	guard, err := mailer.GuardFromConfig(cfg)
	if err != nil {
		t.Fatalf("GuardFromConfig: %v", err)
	}
	var transport mailer.Transport
	if cfg.SendsMail() {
		transport = mailer.SMTPTransport{Addr: cfg.SMTPAddr, Username: cfg.GmailUser, Password: cfg.GmailAppPassword}
	}
	deliverer := mailer.New(guard, transport,
		mail.Address{Name: cfg.FromName, Address: cfg.GmailUser}, quietLogger())

	fetcher, err := dgs.NewExportClient(cfg.DGS, ny, quietLogger())
	if err != nil {
		t.Fatalf("dgs.NewExportClient: %v", err)
	}
	signer, err := applepass.NewSigner(cfg.Apple)
	if err != nil {
		t.Fatalf("applepass.NewSigner: %v", err)
	}
	issuer, err := googlepass.NewIssuer(cfg.Google)
	if err != nil {
		t.Fatalf("googlepass.NewIssuer: %v", err)
	}

	svc := &poll.Service{
		Fetcher: fetcher, Store: db, Mailer: deliverer, Apple: signer, Google: issuer,
		Location: ny, Log: quietLogger(),
	}

	srv, err := server.New(server.Options{Config: cfg, Runner: svc, Store: db, Log: quietLogger(), Version: "integration"})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	appServer := httptest.NewServer(srv.Handler())
	t.Cleanup(appServer.Close)

	// Now that the app's own origin is known, pass links can point at it.
	svc.BaseURL = appServer.URL
	cfg.BaseURL = appServer.URL

	return &stack{
		client: appServer.Client(), pollSvc: svc, baseURL: appServer.URL, smtp: smtpServer,
		store: db, dgsHits: &hits, dbPath: dbPath, location: ny,
	}
}

func (s *stack) poll(t *testing.T, token string) (int, poll.Report) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.baseURL+"/tasks/poll", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("poll request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var report poll.Report
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatalf("decode report (%s): %v", body, err)
		}
	}
	return resp.StatusCode, report
}

func TestPollCycleEndToEndDeliversBadges(t *testing.T) {
	t.Parallel()
	s := newStack(t, config.ModeLive, nil, "")

	status, report := s.poll(t, pollToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	// The export holds three membership rows (MEM/SPON/FNDR) plus one row with a
	// division the club has never used. That row fails classification during
	// ingest, so it never becomes a candidate — it counts as unclassified but not
	// fetched — and three badges go out.
	if report.Fetched != 3 || report.Sent != 3 || report.Unclassified != 1 {
		t.Fatalf("report = %+v", report)
	}

	msgs := s.smtp.Messages()
	if len(msgs) != 3 {
		t.Fatalf("captured %d messages, want 3", len(msgs))
	}

	byRecipient := map[string]smtptest.Message{}
	for _, m := range msgs {
		if len(m.To) != 1 {
			t.Fatalf("message has %d recipients", len(m.To))
		}
		byRecipient[m.To[0]] = m
		if m.AuthUser != "club@gmail.com" || m.AuthPass != "app-password" {
			t.Errorf("SMTP auth = %q/%q, want the app-password credentials", m.AuthUser, m.AuthPass)
		}
		if m.From != "club@gmail.com" {
			t.Errorf("MAIL FROM = %q", m.From)
		}
	}
	for _, want := range []string{"casey@example.com", "robin@example.com", "dana@example.com"} {
		if _, ok := byRecipient[want]; !ok {
			t.Errorf("no message for %s", want)
		}
	}

	// The export has no day passes, so casey's badge is now a season member; the
	// wallet-delivery assertions that used to ride on her day pass move here.
	casey := decodeQP(string(byRecipient["casey@example.com"].Data))
	if !strings.Contains(casey, "Thu, Dec 31 2026 at 11:59 PM EST") {
		t.Error("member email does not carry the Dec 31 expiration")
	}
	if !strings.Contains(casey, "Add to Apple Wallet") || !strings.Contains(casey, "Save to Google Wallet") {
		t.Error("member email is missing wallet buttons")
	}
	if !strings.Contains(casey, "application/vnd.apple.pkpass") {
		t.Error("member email has no .pkpass attachment")
	}

	// Sponsor badges expire with the season, exactly like a member's.
	robin := decodeQP(string(byRecipient["robin@example.com"].Data))
	if !strings.Contains(robin, "Thu, Dec 31 2026 at 11:59 PM EST") {
		t.Error("sponsor email does not carry the Dec 31 expiration")
	}

	// Founder badges never expire, even for a founder who joined at the very end
	// of the season.
	dana := decodeQP(string(byRecipient["dana@example.com"].Data))
	if !strings.Contains(dana, "Never") {
		t.Error("founder email should say the badge never expires")
	}

	// The Apple pass is downloadable from the link in the email.
	caseyID := idFor("casey@example.com")
	art, err := s.store.Artifact(context.Background(), caseyID)
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	resp, err := s.client.Get(s.baseURL + "/passes/" + caseyID + ".pkpass?t=" + art.AccessToken)
	if err != nil {
		t.Fatalf("download pass: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pass download status = %d", resp.StatusCode)
	}
	pkpass, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read pass: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(pkpass), int64(len(pkpass)))
	if err != nil {
		t.Fatalf("downloaded pass is not a zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	for _, required := range []string{"pass.json", "manifest.json", "signature"} {
		if !contains(names, required) {
			t.Errorf("downloaded pass missing %s (has %v)", required, names)
		}
	}
}

func TestReplayedPollCycleSendsNothingNew(t *testing.T) {
	t.Parallel()
	s := newStack(t, config.ModeLive, nil, "")

	if status, _ := s.poll(t, pollToken); status != http.StatusOK {
		t.Fatalf("first poll status = %d", status)
	}
	first := s.smtp.Count()

	status, report := s.poll(t, pollToken)
	if status != http.StatusOK {
		t.Fatalf("second poll status = %d", status)
	}
	if report.AlreadySeen != 3 || report.Sent != 0 {
		t.Fatalf("replay report = %+v, want everything already seen", report)
	}
	if got := s.smtp.Count(); got != first {
		t.Fatalf("replay sent %d extra messages", got-first)
	}
}

func TestPollEndpointRejectsBadToken(t *testing.T) {
	t.Parallel()
	s := newStack(t, config.ModeLive, nil, "")

	for _, token := range []string{"", "wrong-token"} {
		status, _ := s.poll(t, token)
		if status != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", token, status)
		}
	}
	if s.smtp.Count() != 0 {
		t.Error("unauthorized trigger sent mail")
	}
	if s.dgsHits.Load() != 0 {
		t.Error("unauthorized trigger fetched the roster")
	}
}

func TestAllowlistModeMailsOnlyTester(t *testing.T) {
	t.Parallel()
	s := newStack(t, config.ModeAllowlist, []string{"Casey@Example.com"}, "")

	status, report := s.poll(t, pollToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if report.Sent != 1 || report.SkippedGuard != 2 {
		t.Fatalf("report = %+v, want 1 sent and 2 skipped", report)
	}

	msgs := s.smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want only the tester's", len(msgs))
	}
	if msgs[0].To[0] != "casey@example.com" {
		t.Fatalf("mail went to %q", msgs[0].To[0])
	}

	// Suppressed registrants are recorded so a later live run cannot re-mail them.
	for _, email := range []string{"robin@example.com", "dana@example.com"} {
		id := idFor(email)
		rec, err := s.store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if rec.Action != string(mailer.ActionSkipped) || rec.EmailMode != string(config.ModeAllowlist) {
			t.Errorf("%s record = %+v", id, rec)
		}
	}
}

func TestDryRunModeSendsZeroSMTPMessages(t *testing.T) {
	t.Parallel()
	s := newStack(t, config.ModeDryRun, nil, "")

	status, report := s.poll(t, pollToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if report.DryRun != 3 || report.Sent != 0 {
		t.Fatalf("report = %+v, want 3 dry_run", report)
	}
	if s.smtp.Count() != 0 {
		t.Fatalf("dry_run opened %d SMTP transactions, want 0", s.smtp.Count())
	}
	// Passes were still generated, so the run exercised the full pipeline.
	art, err := s.store.Artifact(context.Background(), idFor("casey@example.com"))
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	if len(art.PKPass) == 0 || !strings.HasPrefix(art.GoogleJWT, googlepass.SaveURLPrefix) {
		t.Error("dry_run did not generate wallet passes")
	}
}

func TestRedirectModeSendsEverythingToOneAddress(t *testing.T) {
	t.Parallel()
	s := newStack(t, config.ModeRedirect, nil, "qa@example.com")

	status, report := s.poll(t, pollToken)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if report.Redirected != 3 {
		t.Fatalf("report = %+v, want 3 redirected", report)
	}
	for _, m := range s.smtp.Messages() {
		if len(m.To) != 1 || m.To[0] != "qa@example.com" {
			t.Fatalf("envelope = %v, want the redirect address", m.To)
		}
		if !strings.Contains(decodeQP(string(m.Data)), "would-send:") {
			t.Error("redirected message does not name the intended recipient")
		}
	}
}

func TestWebhookPathDeliversAndDedupes(t *testing.T) {
	t.Parallel()
	s := newStack(t, config.ModeLive, nil, "")

	payload := `{
		"order_id": "DGS-99001",
		"name": "Casey Chains",
		"email": "casey@example.com",
		"item": "Day Pass",
		"purchased_at": "2026-07-04T10:15:00-04:00"
	}`
	post := func() int {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			s.baseURL+"/webhooks/discgolfscene", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer webhook-secret")
		resp, err := s.client.Do(req)
		if err != nil {
			t.Fatalf("webhook request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if got := post(); got != http.StatusAccepted {
		t.Fatalf("first webhook status = %d, want 202", got)
	}
	if got := post(); got != http.StatusOK {
		t.Fatalf("replayed webhook status = %d, want 200", got)
	}
	if got := s.smtp.Count(); got != 1 {
		t.Fatalf("webhook replay produced %d emails, want exactly 1", got)
	}

	rec, err := s.store.Get(context.Background(), "DGS-99001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := time.Date(2026, 7, 5, 23, 59, 59, 0, s.location)
	if !rec.ExpiresAt.Equal(want) {
		t.Errorf("expiry = %s, want %s", rec.ExpiresAt, want)
	}
}

func TestHealthzReportsLiveEmailModeForSmokeTest(t *testing.T) {
	t.Parallel()
	s := newStack(t, config.ModeDryRun, nil, "")

	resp, err := s.client.Get(s.baseURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body server.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.EmailMode != string(config.ModeDryRun) {
		t.Fatalf("email_mode = %q, want dry_run so a mis-set deploy is visible", body.EmailMode)
	}
	if !body.AppleWallet || !body.GoogleWallet || !body.IngestPolling {
		t.Errorf("health body = %+v, want all integrations reported configured", body)
	}
}

func TestSchemaMigrationsSurvivePopulatedVolume(t *testing.T) {
	t.Parallel()
	s := newStack(t, config.ModeLive, nil, "")

	if status, _ := s.poll(t, pollToken); status != http.StatusOK {
		t.Fatal("poll failed")
	}
	before, err := s.store.CountProcessed(context.Background())
	if err != nil {
		t.Fatalf("CountProcessed: %v", err)
	}

	// Reopening the same file re-runs migrations against a populated volume.
	reopened, err := store.Open(s.dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	after, err := reopened.CountProcessed(context.Background())
	if err != nil {
		t.Fatalf("CountProcessed: %v", err)
	}
	if after != before {
		t.Fatalf("processed count changed across reopen: %d -> %d", before, after)
	}
}

func decodeQP(raw string) string {
	return strings.ReplaceAll(strings.ReplaceAll(raw, "=\r\n", ""), "=3D", "=")
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestPipelinePerTier drives one real registration per membership division
// through the full stack: division classification, expiry, artwork, signed
// wallet passes, and the SMTP delivery decision.
func TestPipelinePerTier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		division    string
		wantLabel   string
		wantExpires bool
	}{
		{"member", "MEM", "Season Member", true},
		{"sponsor", "SPON", "Course Sponsor", true},
		{"founder", "FNDR", "Founder", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			passType, err := dgs.ClassifyDivision(tc.division)
			if err != nil {
				t.Fatalf("ClassifyDivision(%q): %v", tc.division, err)
			}
			if passType.Label() != tc.wantLabel {
				t.Fatalf("label = %q, want %q", passType.Label(), tc.wantLabel)
			}

			s := newStack(t, config.ModeLive, nil, "")
			reg := domain.Registration{
				ID:          "reg-" + tc.name,
				Name:        "Test " + tc.name,
				Email:       tc.name + "@example.com",
				RawPassType: tc.division,
				SeasonYear:  2026,
				// A 2026-season purchase made in November 2025.
				PurchasedAt: time.Date(2025, 11, 13, 1, 7, 27, 0, s.location),
			}
			outcome, err := s.pollSvc.ProcessClassified(context.Background(), reg, passType)
			if err != nil {
				t.Fatalf("ProcessClassified: %v", err)
			}
			if got := !outcome.ExpiresAt.IsZero(); got != tc.wantExpires {
				t.Fatalf("expires = %v, want %v", got, tc.wantExpires)
			}
			if tc.wantExpires && outcome.ExpiresAt.Year() != 2026 {
				t.Errorf("expiry year = %d, want 2026 (season year, not purchase year)", outcome.ExpiresAt.Year())
			}
			if outcome.Action != mailer.ActionSent {
				t.Fatalf("action = %q, want %q", outcome.Action, mailer.ActionSent)
			}
			if len(s.smtp.Messages()) != 1 {
				t.Fatalf("sent %d emails, want 1", len(s.smtp.Messages()))
			}
			body := decodeQP(string(s.smtp.Messages()[0].Data))
			if !strings.Contains(body, tc.wantLabel) {
				t.Errorf("email does not name the %s tier", tc.wantLabel)
			}
			if !tc.wantExpires && !strings.Contains(body, "Never") {
				t.Error("founder email should say the badge never expires")
			}
		})
	}
}
