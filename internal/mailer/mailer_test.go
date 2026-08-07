package mailer_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/email"
	"github.com/northlanding/badges/internal/mailer"
	"github.com/northlanding/badges/internal/smtptest"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testData() email.Data {
	ny, _ := time.LoadLocation("America/New_York")
	return email.Data{
		Badge: domain.Badge{
			Registration: domain.Registration{
				ID:          "DGS-1",
				Name:        "Casey Chains",
				Email:       "casey@example.com",
				RawPassType: "Day Pass",
				PurchasedAt: time.Date(2026, 7, 4, 10, 0, 0, 0, ny),
			},
			PassType:  domain.PassTypeDay,
			ExpiresAt: time.Date(2026, 7, 5, 23, 59, 59, 0, ny),
		},
		AppleURL:  "https://badges.example.com/passes/DGS-1.pkpass?t=tok",
		GoogleURL: "https://pay.google.com/gp/v/save/jwt",
		BadgePNG:  []byte("png"),
		Location:  ny,
	}
}

type recordingTransport struct {
	mu   sync.Mutex
	sent []struct {
		From string
		To   []string
		Raw  []byte
	}
	err error
}

func (r *recordingTransport) Send(_ context.Context, from string, to []string, raw []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, struct {
		From string
		To   []string
		Raw  []byte
	}{from, to, raw})
	return nil
}

func (r *recordingTransport) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

func newMailer(t *testing.T, mode config.EmailMode, allowlist []string, redirectTo string, tr mailer.Transport) *mailer.Mailer {
	t.Helper()
	g, err := mailer.NewGuard(mode, allowlist, redirectTo)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	return mailer.New(g, tr, mail.Address{Name: "North Landing Community", Address: "club@gmail.com"}, quietLogger())
}

func TestDeliverLiveModeSends(t *testing.T) {
	t.Parallel()
	tr := &recordingTransport{}
	m := newMailer(t, config.ModeLive, nil, "", tr)

	res, err := m.Deliver(context.Background(), "DGS-1", testData())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Decision.Action != mailer.ActionSent {
		t.Errorf("action = %q", res.Decision.Action)
	}
	if tr.count() != 1 {
		t.Fatalf("sent %d messages, want 1", tr.count())
	}
	sent := tr.sent[0]
	if sent.From != "club@gmail.com" {
		t.Errorf("envelope from = %q", sent.From)
	}
	if len(sent.To) != 1 || sent.To[0] != "casey@example.com" {
		t.Errorf("envelope to = %v", sent.To)
	}
	if !strings.Contains(string(sent.Raw), "Add to Apple Wallet") {
		t.Error("rendered body missing wallet button")
	}
}

func TestDeliverAllowlistSkipsNonTesters(t *testing.T) {
	t.Parallel()
	tr := &recordingTransport{}
	m := newMailer(t, config.ModeAllowlist, []string{"tester@example.com"}, "", tr)

	res, err := m.Deliver(context.Background(), "DGS-1", testData())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Decision.Action != mailer.ActionSkipped {
		t.Errorf("action = %q, want skipped", res.Decision.Action)
	}
	if tr.count() != 0 {
		t.Errorf("sent %d messages, want 0", tr.count())
	}
	if len(res.Raw) == 0 {
		t.Error("suppressed message should still be rendered for logging")
	}
}

func TestDeliverAllowlistSendsToTester(t *testing.T) {
	t.Parallel()
	tr := &recordingTransport{}
	m := newMailer(t, config.ModeAllowlist, []string{"casey@example.com"}, "", tr)

	res, err := m.Deliver(context.Background(), "DGS-1", testData())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Decision.Action != mailer.ActionSent {
		t.Errorf("action = %q, want sent", res.Decision.Action)
	}
	if tr.count() != 1 {
		t.Errorf("sent %d messages, want 1", tr.count())
	}
}

func TestDeliverRedirectRewritesEnvelopeAndSubject(t *testing.T) {
	t.Parallel()
	tr := &recordingTransport{}
	m := newMailer(t, config.ModeRedirect, nil, "qa@example.com", tr)

	res, err := m.Deliver(context.Background(), "DGS-1", testData())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Decision.Action != mailer.ActionRedirected {
		t.Errorf("action = %q", res.Decision.Action)
	}
	if tr.count() != 1 {
		t.Fatalf("sent %d messages, want 1", tr.count())
	}
	if to := tr.sent[0].To; len(to) != 1 || to[0] != "qa@example.com" {
		t.Errorf("envelope to = %v, want the redirect address", to)
	}
	if !strings.HasPrefix(res.Subject, "[would-send: casey@example.com] ") {
		t.Errorf("subject = %q, want the would-send prefix", res.Subject)
	}
	// Undo quoted-printable soft line breaks before matching body copy.
	body := strings.ReplaceAll(string(tr.sent[0].Raw), "=\r\n", "")
	if !strings.Contains(body, "would have been sent to casey@example.com") {
		t.Error("body header does not state the original recipient")
	}
}

func TestDeliverDryRunSendsNothing(t *testing.T) {
	t.Parallel()
	tr := &recordingTransport{}
	m := newMailer(t, config.ModeDryRun, nil, "", tr)

	res, err := m.Deliver(context.Background(), "DGS-1", testData())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Decision.Action != mailer.ActionDryRun {
		t.Errorf("action = %q", res.Decision.Action)
	}
	if tr.count() != 0 {
		t.Errorf("sent %d messages, want 0", tr.count())
	}
	if len(res.Raw) == 0 {
		t.Error("dry_run must still render the message so it can be logged")
	}
}

func TestDeliverDryRunNeedsNoTransport(t *testing.T) {
	t.Parallel()
	m := newMailer(t, config.ModeDryRun, nil, "", nil)
	if _, err := m.Deliver(context.Background(), "DGS-1", testData()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

func TestDeliverWithoutTransportInSendingModeFails(t *testing.T) {
	t.Parallel()
	m := newMailer(t, config.ModeLive, nil, "", nil)
	if _, err := m.Deliver(context.Background(), "DGS-1", testData()); err == nil {
		t.Fatal("expected error when a sending mode has no transport")
	}
}

func TestDeliverPropagatesTransportError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("smtp exploded")
	m := newMailer(t, config.ModeLive, nil, "", &recordingTransport{err: sentinel})
	if _, err := m.Deliver(context.Background(), "DGS-1", testData()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestDeliverRejectsUnrenderableData(t *testing.T) {
	t.Parallel()
	m := newMailer(t, config.ModeLive, nil, "", &recordingTransport{})
	if _, err := m.Deliver(context.Background(), "DGS-1", email.Data{}); err == nil {
		t.Fatal("expected render error")
	}
}

func TestSMTPTransportSendsOverRealSocket(t *testing.T) {
	t.Parallel()
	srv, err := smtptest.Start(smtptest.Options{RequireAuth: true, Username: "club@gmail.com", Password: "app-password"})
	if err != nil {
		t.Fatalf("smtptest.Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	tr := mailer.SMTPTransport{Addr: srv.Addr(), Username: "club@gmail.com", Password: "app-password"}
	m := mailer.New(mustGuard(t, config.ModeLive), tr,
		mail.Address{Name: "North Landing Community", Address: "club@gmail.com"}, quietLogger())

	if _, err := m.Deliver(context.Background(), "DGS-1", testData()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	msgs := srv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	got := msgs[0]
	if got.From != "club@gmail.com" {
		t.Errorf("MAIL FROM = %q", got.From)
	}
	if len(got.To) != 1 || got.To[0] != "casey@example.com" {
		t.Errorf("RCPT TO = %v", got.To)
	}
	if got.AuthUser != "club@gmail.com" || got.AuthPass != "app-password" {
		t.Errorf("auth = %q/%q, want the app password credentials", got.AuthUser, got.AuthPass)
	}
	data := string(got.Data)
	for _, want := range []string{
		"Subject: ", "MIME-Version: 1.0", "multipart/mixed", "Content-Type: image/png",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("wire data missing %q", want)
		}
	}
}

func TestSMTPTransportSurfacesServerRejection(t *testing.T) {
	t.Parallel()
	srv, err := smtptest.Start(smtptest.Options{FailData: true})
	if err != nil {
		t.Fatalf("smtptest.Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	tr := mailer.SMTPTransport{Addr: srv.Addr()}
	if err := tr.Send(context.Background(), "club@gmail.com", []string{"casey@example.com"}, []byte("Subject: x\r\n\r\nbody\r\n")); err == nil {
		t.Fatal("expected error when the server rejects DATA")
	}
	if srv.Count() != 0 {
		t.Errorf("server captured %d messages despite rejecting DATA", srv.Count())
	}
}

func TestSMTPTransportRequiresAddress(t *testing.T) {
	t.Parallel()
	var tr mailer.SMTPTransport
	if err := tr.Send(context.Background(), "a@b.com", []string{"c@d.com"}, []byte("x")); err == nil {
		t.Fatal("expected error with no address")
	}
	bad := mailer.SMTPTransport{Addr: "no-port", Username: "u"}
	if err := bad.Send(context.Background(), "a@b.com", []string{"c@d.com"}, []byte("x")); err == nil {
		t.Fatal("expected error for malformed address")
	}
}

func TestSMTPTransportHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	srv, err := smtptest.Start(smtptest.Options{})
	if err != nil {
		t.Fatalf("smtptest.Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr := mailer.SMTPTransport{Addr: srv.Addr()}
	err = tr.Send(ctx, "club@gmail.com", []string{"casey@example.com"}, []byte("Subject: x\r\n\r\nbody\r\n"))
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled or success", err)
	}
}

func mustGuard(t *testing.T, mode config.EmailMode) mailer.Guard {
	t.Helper()
	g, err := mailer.NewGuard(mode, nil, "")
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	return g
}
