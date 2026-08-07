package poll_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/mailer"
	"github.com/northlanding/badges/internal/poll"
	"github.com/northlanding/badges/internal/store"
	"github.com/northlanding/badges/internal/testkeys"
	"github.com/northlanding/badges/internal/wallet/applepass"
	"github.com/northlanding/badges/internal/wallet/googlepass"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func clubTZ(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

type fakeFetcher struct {
	candidates []domain.Candidate
	errs       []error
	hits       atomic.Int64
}

func (f *fakeFetcher) Fetch(context.Context) ([]domain.Candidate, []error) {
	f.hits.Add(1)
	return f.candidates, f.errs
}

type recordingTransport struct {
	mu   sync.Mutex
	sent [][]string
	raw  [][]byte
	err  error
}

func (r *recordingTransport) Send(_ context.Context, _ string, to []string, raw []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, to)
	r.raw = append(r.raw, raw)
	return nil
}

func (r *recordingTransport) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

// decodeQP undoes the quoted-printable encoding applied to text bodies so tests
// can match on the copy as a reader would see it.
func decodeQP(raw string) string {
	return strings.ReplaceAll(strings.ReplaceAll(raw, "=\r\n", ""), "=3D", "=")
}

func dayPass(id, email string, purchased time.Time) domain.Registration {
	return domain.Registration{
		ID: id, Name: "Casey Chains", Email: email,
		RawPassType: "Day Pass", PurchasedAt: purchased,
	}
}

// dayCandidate pairs a day-pass registration with the pass type ingest would
// have classified it to, since the fetcher now hands the pipeline candidates
// rather than raw registrations.
func dayCandidate(id, email string, purchased time.Time) domain.Candidate {
	return domain.Candidate{Registration: dayPass(id, email, purchased), PassType: domain.PassTypeDay}
}

type harness struct {
	svc       *poll.Service
	store     *store.Store
	transport *recordingTransport
	fetcher   *fakeFetcher
}

func newHarness(t *testing.T, mode config.EmailMode, allowlist []string, redirectTo string, candidates []domain.Candidate, ingestErrs []error) *harness {
	t.Helper()
	ny := clubTZ(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "badges.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	guard, err := mailer.NewGuard(mode, allowlist, redirectTo)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	tr := &recordingTransport{}
	m := mailer.New(guard, tr, mail.Address{Name: "North Landing Community", Address: "club@gmail.com"}, quietLogger())

	signer, err := applepass.NewSigner(config.AppleConfig{
		PassTypeIdentifier: "pass.com.northlanding.badge",
		TeamIdentifier:     "TESTTEAM01",
		OrganizationName:   "North Landing Community",
		CertPEM:            testkeys.ApplePassCertPEM(),
		KeyPEM:             testkeys.ApplePassKeyPEM(),
		WWDRPEM:            testkeys.AppleWWDRPEM(),
	})
	if err != nil {
		t.Fatalf("applepass.NewSigner: %v", err)
	}
	issuer, err := googlepass.NewIssuer(config.GoogleConfig{
		IssuerID:            "3388000000012345678",
		ClassID:             "3388000000012345678.north-landing-badge",
		ServiceAccountEmail: "wallet@north-landing.iam.gserviceaccount.com",
		KeyPEM:              testkeys.GoogleServiceAccountKeyPEM(),
	})
	if err != nil {
		t.Fatalf("googlepass.NewIssuer: %v", err)
	}

	fetcher := &fakeFetcher{candidates: candidates, errs: ingestErrs}
	return &harness{
		svc: &poll.Service{
			Fetcher: fetcher, Store: st, Mailer: m, Apple: signer, Google: issuer,
			Location: ny, BaseURL: "https://badges.example.com", Log: quietLogger(),
		},
		store: st, transport: tr, fetcher: fetcher,
	}
}

func TestRunCycleEndToEnd(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeLive, nil, "", []domain.Candidate{
		dayCandidate("DGS-1", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
		{
			Registration: domain.Registration{
				ID: "DGS-2", Name: "Robin Rollaway", Email: "robin@example.com",
				RawPassType: "2026 Season Membership", SeasonYear: 2026,
				PurchasedAt: time.Date(2026, 4, 1, 8, 0, 0, 0, ny),
			},
			PassType: domain.PassTypeSeason,
		},
	}, nil)

	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.Fetched != 2 || report.Processed != 2 || report.Sent != 2 {
		t.Fatalf("report = %+v", report)
	}
	if h.transport.count() != 2 {
		t.Fatalf("sent %d emails, want 2", h.transport.count())
	}

	day, err := h.store.Get(context.Background(), "DGS-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if day.Action != "sent" || day.EmailMode != "live" {
		t.Errorf("day pass record = %+v", day)
	}
	if want := time.Date(2026, 7, 5, 23, 59, 59, 0, ny); !day.ExpiresAt.Equal(want) {
		t.Errorf("day pass expiry = %s, want %s", day.ExpiresAt, want)
	}

	season, err := h.store.Get(context.Background(), "DGS-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if want := time.Date(2026, 12, 31, 23, 59, 59, 0, ny); !season.ExpiresAt.Equal(want) {
		t.Errorf("season expiry = %s, want %s", season.ExpiresAt, want)
	}
	if season.PassType != string(domain.PassTypeSeason) {
		t.Errorf("season pass type = %q", season.PassType)
	}

	// The Apple pass must be stored and linked from the email.
	art, err := h.store.Artifact(context.Background(), "DGS-1")
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	if len(art.PKPass) == 0 {
		t.Error("no .pkpass stored")
	}
	if !strings.HasPrefix(art.GoogleJWT, googlepass.SaveURLPrefix) {
		t.Errorf("stored google link = %q", art.GoogleJWT)
	}
	body := decodeQP(string(h.transport.raw[0]))
	if !strings.Contains(body, "https://badges.example.com/passes/DGS-1.pkpass?t="+art.AccessToken) {
		t.Error("email does not link the stored pass download URL")
	}
}

func TestRunCycleIsIdempotent(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeLive, nil, "", []domain.Candidate{
		dayCandidate("DGS-1", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
	}, nil)

	if _, err := h.svc.RunCycle(context.Background()); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if report.AlreadySeen != 1 || report.Processed != 0 || report.Sent != 0 {
		t.Fatalf("replay report = %+v, want the registration recognised as already seen", report)
	}
	if h.transport.count() != 1 {
		t.Fatalf("sent %d emails across two cycles, want exactly 1", h.transport.count())
	}
}

func TestConcurrentCyclesSendOnce(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeLive, nil, "", []domain.Candidate{
		dayCandidate("DGS-RACE", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
	}, nil)

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := h.svc.RunCycle(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent cycle failed: %v", err)
	}

	if got := h.transport.count(); got != 1 {
		t.Fatalf("sent %d emails from concurrent cycles, want exactly 1", got)
	}
}

func TestRunCycleAllowlistMailsOnlyTester(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeAllowlist, []string{"tester@example.com"}, "", []domain.Candidate{
		dayCandidate("DGS-1", "tester@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
		dayCandidate("DGS-2", "real@user.com", time.Date(2026, 7, 4, 11, 0, 0, 0, ny)),
		dayCandidate("DGS-3", "another@user.com", time.Date(2026, 7, 4, 12, 0, 0, 0, ny)),
	}, nil)

	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.Sent != 1 || report.SkippedGuard != 2 {
		t.Fatalf("report = %+v, want 1 sent and 2 skipped", report)
	}
	if h.transport.count() != 1 {
		t.Fatalf("sent %d emails, want 1", h.transport.count())
	}
	if to := h.transport.sent[0]; len(to) != 1 || to[0] != "tester@example.com" {
		t.Fatalf("envelope = %v, want only the tester", to)
	}

	// Suppressed registrations are still recorded, so a later live run cannot
	// re-mail them without clearing the ledger.
	for _, id := range []string{"DGS-2", "DGS-3"} {
		rec, err := h.store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if rec.Action != string(mailer.ActionSkipped) {
			t.Errorf("%s action = %q, want skipped", id, rec.Action)
		}
	}
}

func TestRunCycleDryRunSendsNothingButGeneratesPasses(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeDryRun, nil, "", []domain.Candidate{
		dayCandidate("DGS-1", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
		dayCandidate("DGS-2", "robin@example.com", time.Date(2026, 7, 4, 11, 0, 0, 0, ny)),
	}, nil)

	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.DryRun != 2 || report.Sent != 0 {
		t.Fatalf("report = %+v, want 2 dry_run and 0 sent", report)
	}
	if h.transport.count() != 0 {
		t.Fatalf("dry_run sent %d SMTP messages, want 0", h.transport.count())
	}
	// Full pipeline still ran: passes exist.
	for _, id := range []string{"DGS-1", "DGS-2"} {
		art, err := h.store.Artifact(context.Background(), id)
		if err != nil {
			t.Fatalf("Artifact(%s): %v", id, err)
		}
		if len(art.PKPass) == 0 || art.GoogleJWT == "" {
			t.Errorf("%s passes not generated in dry_run", id)
		}
	}
}

func TestRunCycleRedirectsEveryMessage(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeRedirect, nil, "qa@example.com", []domain.Candidate{
		dayCandidate("DGS-1", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
		dayCandidate("DGS-2", "robin@example.com", time.Date(2026, 7, 4, 11, 0, 0, 0, ny)),
	}, nil)

	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.Redirected != 2 {
		t.Fatalf("report = %+v, want 2 redirected", report)
	}
	for i, to := range h.transport.sent {
		if len(to) != 1 || to[0] != "qa@example.com" {
			t.Fatalf("envelope[%d] = %v, want the redirect address", i, to)
		}
	}
}

// TestRunCycleReportsUnclassifiedWithoutClaiming covers RunCycle's own defense
// against a candidate that carries a pass type ProcessClassified does not
// recognize. Ingest is now the only place that classifies a raw label, so this
// can no longer happen from a bad RawPassType — but a Candidate is still a
// value the fetcher hands over, and a bug there must not silently claim (and
// thus never retry) the registration.
func TestRunCycleReportsUnclassifiedWithoutClaiming(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeLive, nil, "", []domain.Candidate{
		{
			Registration: domain.Registration{
				ID: "DGS-T", Name: "Jamie Jomez", Email: "jamie@example.com",
				RawPassType: "Tournament Entry", PurchasedAt: time.Date(2026, 6, 12, 17, 30, 0, 0, ny),
			},
			PassType: domain.PassType("tournament_entry"),
		},
		dayCandidate("DGS-1", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
	}, nil)

	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.Unclassified != 1 || report.Sent != 1 {
		t.Fatalf("report = %+v, want 1 unclassified and 1 sent", report)
	}
	// An unclassifiable registration must not be marked processed: if the club
	// fixes the item label, the next cycle should pick it up.
	if seen, err := h.store.Processed(context.Background(), "DGS-T"); err != nil || seen {
		t.Errorf("unclassified registration was claimed (seen=%v, err=%v)", seen, err)
	}
}

func TestRunCycleReleasesClaimWhenDeliveryFails(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeLive, nil, "", []domain.Candidate{
		dayCandidate("DGS-1", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
	}, nil)
	h.transport.err = errors.New("smtp down")

	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle should report per-registration failures, not fail: %v", err)
	}
	if report.Failed != 1 || report.Processed != 0 {
		t.Fatalf("report = %+v, want 1 failure", report)
	}
	if seen, err := h.store.Processed(context.Background(), "DGS-1"); err != nil || seen {
		t.Errorf("failed registration stayed claimed (seen=%v, err=%v)", seen, err)
	}

	// Once SMTP recovers, the next cycle delivers it.
	h.transport.err = nil
	report, err = h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("retry cycle: %v", err)
	}
	if report.Sent != 1 {
		t.Fatalf("retry report = %+v, want 1 sent", report)
	}
}

func TestRunCycleWithNoRegistrationsIsANoOp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, config.ModeLive, nil, "", nil, nil)
	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.Fetched != 0 || report.Processed != 0 || report.Sent != 0 || report.AlreadySeen != 0 ||
		report.Failed != 0 || report.Unclassified != 0 || len(report.Errors) != 0 || len(report.IngestWarning) != 0 {
		t.Fatalf("report = %+v, want an all-zero no-op report", report)
	}
	if h.transport.count() != 0 {
		t.Error("empty cycle sent mail")
	}
}

func TestRunCycleSurfacesIngestFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t, config.ModeLive, nil, "", nil, []error{errors.New("roster returned 403")})
	if _, err := h.svc.RunCycle(context.Background()); err == nil {
		t.Fatal("expected an error when ingest fails and yields nothing")
	}
}

func TestRunCycleKeepsGoingAfterRowWarnings(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeLive, nil, "", []domain.Candidate{
		dayCandidate("DGS-1", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
	}, []error{errors.New("row 4: missing email")})

	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.Sent != 1 {
		t.Fatalf("report = %+v, want the good row delivered", report)
	}
	if len(report.IngestWarning) != 1 {
		t.Errorf("ingest warnings = %v, want 1", report.IngestWarning)
	}
}

func TestProcessRejectsInvalidRegistration(t *testing.T) {
	t.Parallel()
	h := newHarness(t, config.ModeLive, nil, "", nil, nil)
	if _, err := h.svc.Process(context.Background(), domain.Registration{}); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRunCycleRequiresDependencies(t *testing.T) {
	t.Parallel()
	var svc poll.Service
	if _, err := svc.RunCycle(context.Background()); err == nil {
		t.Error("expected error without a fetcher")
	}

	ny := clubTZ(t)
	noStore := &poll.Service{Fetcher: &fakeFetcher{}, Log: quietLogger(), Location: ny}
	if _, err := noStore.Process(context.Background(),
		dayPass("DGS-1", "c@x.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny))); err == nil {
		t.Error("expected error without a store")
	}
}

func TestRunCycleWithoutWalletBuildersStillMails(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeLive, nil, "", []domain.Candidate{
		dayCandidate("DGS-1", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
	}, nil)
	h.svc.Apple = nil
	h.svc.Google = nil

	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.Sent != 1 {
		t.Fatalf("report = %+v", report)
	}
	if _, err := h.store.Artifact(context.Background(), "DGS-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected no artifact without wallet builders, err = %v", err)
	}
}

func TestRunCycleStopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeLive, nil, "", []domain.Candidate{
		dayCandidate("DGS-1", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny)),
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.svc.RunCycle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if h.transport.count() != 0 {
		t.Error("cancelled cycle sent mail")
	}
}

func TestProcessClassifiedSkipsSecondFounderBadge(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	ctx := context.Background()
	h := newHarness(t, config.ModeLive, nil, "", nil, nil)

	first := domain.Registration{
		ID: "reg-2025", Name: "A Founder", Email: "founder@example.com",
		RawPassType: "FNDR", SeasonYear: 2025,
		PurchasedAt: time.Date(2024, 11, 20, 9, 0, 0, 0, ny),
	}
	if _, err := h.svc.ProcessClassified(ctx, first, domain.PassTypeFounder); err != nil {
		t.Fatalf("first founder registration: %v", err)
	}

	second := domain.Registration{
		ID: "reg-2026", Name: "A Founder", Email: "founder@example.com",
		RawPassType: "FNDR", SeasonYear: 2026,
		PurchasedAt: time.Date(2025, 11, 13, 1, 7, 27, 0, ny),
	}
	outcome, err := h.svc.ProcessClassified(ctx, second, domain.PassTypeFounder)
	if err != nil {
		t.Fatalf("second founder registration: %v", err)
	}
	if outcome.Action != mailer.ActionFounderExisting {
		t.Errorf("action = %q, want %q", outcome.Action, mailer.ActionFounderExisting)
	}

	rec, err := h.store.Get(ctx, "reg-2026")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Action != string(mailer.ActionFounderExisting) {
		t.Errorf("stored action = %q, want %q", rec.Action, mailer.ActionFounderExisting)
	}
	if h.transport.count() != 1 {
		t.Errorf("emails sent = %d, want 1", h.transport.count())
	}
}

func TestProcessClassifiedSeasonUsesRegistrationSeasonYear(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeLive, nil, "", nil, nil)

	reg := domain.Registration{
		ID: "reg-1", Name: "A Member", Email: "m@example.com",
		RawPassType: "MEM", SeasonYear: 2026,
		PurchasedAt: time.Date(2025, 11, 13, 1, 7, 27, 0, ny),
	}
	outcome, err := h.svc.ProcessClassified(context.Background(), reg, domain.PassTypeSeason)
	if err != nil {
		t.Fatalf("ProcessClassified: %v", err)
	}
	if got := outcome.ExpiresAt.Year(); got != 2026 {
		t.Errorf("expiry year = %d, want 2026 (season year, not purchase year)", got)
	}
}

func TestRunCycleUsesCandidatePassTypes(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	h := newHarness(t, config.ModeLive, nil, "", []domain.Candidate{
		{
			Registration: domain.Registration{
				ID: "abc123", Name: "Dana Discraft", Email: "dana@example.com",
				RawPassType: "FNDR", SeasonYear: 2026,
				PurchasedAt: time.Date(2025, 11, 13, 1, 7, 26, 0, ny),
			},
			PassType: domain.PassTypeFounder,
		},
		{
			Registration: domain.Registration{
				ID: "def456", Name: "Sam Sponsor", Email: "sam@example.com",
				RawPassType: "SPON", SeasonYear: 2026,
				PurchasedAt: time.Date(2026, 2, 20, 19, 45, 0, 0, ny),
			},
			PassType: domain.PassTypeSponsor,
		},
	}, nil)

	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.Fetched != 2 || report.Processed != 2 || report.Sent != 2 {
		t.Fatalf("report = %+v", report)
	}

	founder, err := h.store.Get(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if founder.PassType != string(domain.PassTypeFounder) {
		t.Errorf("founder pass type = %q", founder.PassType)
	}
	if !founder.ExpiresAt.IsZero() {
		t.Errorf("founder expiry = %s, want zero", founder.ExpiresAt)
	}

	sponsor, err := h.store.Get(context.Background(), "def456")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if want := time.Date(2026, 12, 31, 23, 59, 59, 0, ny); !sponsor.ExpiresAt.Equal(want) {
		t.Errorf("sponsor expiry = %s, want %s", sponsor.ExpiresAt, want)
	}
}

func TestRunCycleCountsUnknownDivisionAsUnclassified(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	// A good candidate rides along with the bad row: RunCycle's hard-failure path
	// (no candidates, only ingest errors) only triggers when the fetch produced
	// nothing usable at all. With one good candidate present, RunCycle returns a
	// nil error and actually walks the accounting logic this test exists to
	// cover, rather than short-circuiting before it ever gets there.
	h := newHarness(t, config.ModeLive, nil, "",
		[]domain.Candidate{dayCandidate("DGS-1", "casey@example.com", time.Date(2026, 7, 4, 10, 0, 0, 0, ny))},
		[]error{fmt.Errorf("row 6: %w: division %q", domain.ErrUnknownPassType, "PRO")})

	report, err := h.svc.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if report.Unclassified != 1 {
		t.Errorf("report.Unclassified = %d, want 1", report.Unclassified)
	}
	if report.Processed != 1 {
		t.Errorf("report.Processed = %d, want 1 (the good candidate must still go through)", report.Processed)
	}
}
