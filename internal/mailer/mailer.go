package mailer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/email"
)

// Transport puts a serialized message on the wire.
type Transport interface {
	Send(ctx context.Context, from string, to []string, raw []byte) error
}

// SMTPTransport sends through an SMTP submission server — in production, Gmail
// authenticated with a Google App Password.
type SMTPTransport struct {
	Addr     string
	Username string
	Password string
}

// Send performs one SMTP transaction. STARTTLS is negotiated by net/smtp
// whenever the server advertises it, which Gmail always does.
func (t SMTPTransport) Send(ctx context.Context, from string, to []string, raw []byte) error {
	if t.Addr == "" {
		return fmt.Errorf("mailer: no SMTP address configured")
	}
	var auth smtp.Auth
	if t.Username != "" {
		host, _, err := net.SplitHostPort(t.Addr)
		if err != nil {
			return fmt.Errorf("mailer: SMTP address %q: %w", t.Addr, err)
		}
		auth = smtp.PlainAuth("", t.Username, t.Password, host)
	}

	// net/smtp has no context support; run it on a goroutine so a cancelled
	// context stops the caller waiting on a hung server.
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(t.Addr, auth, from, to, raw) }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("mailer: smtp send: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("mailer: smtp send: %w", ctx.Err())
	}
}

// Mailer renders and conditionally delivers badge emails.
type Mailer struct {
	guard     Guard
	transport Transport
	from      mail.Address
	log       *slog.Logger
	clock     func() time.Time
}

// New builds a Mailer. A nil transport is valid only in dry_run mode.
func New(guard Guard, transport Transport, from mail.Address, logger *slog.Logger) *Mailer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Mailer{guard: guard, transport: transport, from: from, log: logger, clock: time.Now}
}

// Mode reports the active delivery guard mode.
func (m *Mailer) Mode() config.EmailMode { return m.guard.Mode() }

// Result describes what happened to one registration's email.
type Result struct {
	Decision Decision
	Subject  string
	// Raw is the serialized message. Populated even when nothing was sent, so
	// dry_run and skipped runs can log exactly what would have gone out.
	Raw []byte
}

// Deliver renders the message for a registration and applies the guard.
//
// Rendering always happens: per spec §4 the guard gates only the SMTP send, so a
// guarded run still exercises badge generation, pass signing and rendering.
func (m *Mailer) Deliver(ctx context.Context, registrationID string, data email.Data) (Result, error) {
	decision := m.guard.Decide(data.Badge.Registration.Email)
	data.Notice = decision.BodyNotice
	data.SubjectPrefix = decision.SubjectPrefix

	msg, err := email.Render(data)
	if err != nil {
		return Result{Decision: decision}, err
	}

	recipients := decision.Recipients
	if len(recipients) == 0 {
		// Nothing will be sent, but serialize anyway so the log carries the exact
		// message body that was suppressed.
		recipients = []string{decision.IntendedRecipient}
	}
	raw, err := msg.MIME(email.MIMEOptions{
		From:      m.from,
		To:        recipients,
		Date:      m.clock(),
		MessageID: m.messageID(registrationID),
	})
	if err != nil {
		return Result{Decision: decision, Subject: msg.Subject}, err
	}
	result := Result{Decision: decision, Subject: msg.Subject, Raw: raw}

	logAttrs := []any{
		"registration_id", registrationID,
		"intended_recipient", decision.IntendedRecipient,
		"email_mode", string(decision.Mode),
		"action", string(decision.Action),
	}

	if !decision.Send() {
		if decision.Action == ActionDryRun {
			m.log.Info("delivery guard suppressed email", append(logAttrs,
				"subject", msg.Subject, "bytes", len(raw))...)
		} else {
			m.log.Info("delivery guard suppressed email", append(logAttrs, "subject", msg.Subject)...)
		}
		return result, nil
	}

	if m.transport == nil {
		return result, fmt.Errorf("mailer: no transport configured but mode %q requires sending", decision.Mode)
	}
	if err := m.transport.Send(ctx, m.from.Address, decision.Recipients, raw); err != nil {
		m.log.Error("email send failed", append(logAttrs, "error", err)...)
		return result, err
	}
	m.log.Info("email delivered", append(logAttrs,
		"envelope_to", strings.Join(decision.Recipients, ","), "subject", msg.Subject)...)
	return result, nil
}

func (m *Mailer) messageID(registrationID string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s@northlanding.badges", sanitizeMessageIDPart(registrationID))
	}
	return fmt.Sprintf("%s.%s@northlanding.badges", sanitizeMessageIDPart(registrationID), hex.EncodeToString(buf[:]))
}

func sanitizeMessageIDPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "badge"
	}
	return b.String()
}
