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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/dgs"
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

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type stack struct {
	client   *http.Client
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

	// Recorded DiscGolfScene orders page.
	fixture, err := os.ReadFile(filepath.Join("..", "dgs", "testdata", "orders.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var hits atomic.Int32
	dgsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(dgsServer.Close)

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
		DGS:              config.DGSConfig{RosterURL: dgsServer.URL},
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

	fetcher, err := dgs.NewClient(cfg.DGS, ny, quietLogger())
	if err != nil {
		t.Fatalf("dgs.NewClient: %v", err)
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
		client: appServer.Client(), baseURL: appServer.URL, smtp: smtpServer,
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
	// The fixture holds four parseable rows; one is a tournament entry that must
	// not classify, so three badges go out.
	if report.Fetched != 4 || report.Sent != 3 || report.Unclassified != 1 {
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

	// Day Pass expiration is purchase date + 1 day at 23:59:59 club-local.
	casey := decodeQP(string(byRecipient["casey@example.com"].Data))
	if !strings.Contains(casey, "Sun, Jul 5 2026 at 11:59 PM EDT") {
		t.Error("day pass email does not carry the calculated expiration")
	}
	if !strings.Contains(casey, "Add to Apple Wallet") || !strings.Contains(casey, "Save to Google Wallet") {
		t.Error("day pass email is missing wallet buttons")
	}
	if !strings.Contains(casey, "application/vnd.apple.pkpass") {
		t.Error("day pass email has no .pkpass attachment")
	}

	// Season membership expires Dec 31 of the purchase year.
	robin := decodeQP(string(byRecipient["robin@example.com"].Data))
	if !strings.Contains(robin, "Thu, Dec 31 2026 at 11:59 PM EST") {
		t.Error("season email does not carry the Dec 31 expiration")
	}

	// A Dec 31 purchase still expires that same Dec 31.
	dana := decodeQP(string(byRecipient["dana@example.com"].Data))
	if !strings.Contains(dana, "Thu, Dec 31 2026 at 11:59 PM EST") {
		t.Error("year-end season purchase has the wrong expiration")
	}

	// The Apple pass is downloadable from the link in the email.
	art, err := s.store.Artifact(context.Background(), "DGS-88231")
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	resp, err := s.client.Get(s.baseURL + "/passes/DGS-88231.pkpass?t=" + art.AccessToken)
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
	for _, id := range []string{"DGS-88232", "DGS-88233"} {
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
	art, err := s.store.Artifact(context.Background(), "DGS-88231")
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
