// Package mailer applies the delivery guard (spec §4) and puts messages on the
// wire over SMTP.
package mailer

import (
	"errors"
	"fmt"

	"github.com/northlanding/badges/internal/config"
)

// Action is the outcome recorded for a guarded delivery decision.
type Action string

const (
	// ActionSent means the message goes to the real registrant.
	ActionSent Action = "sent"
	// ActionSkipped means the recipient is not on the allowlist; nothing is sent.
	ActionSkipped Action = "skipped"
	// ActionRedirected means the message goes to EMAIL_REDIRECT_TO instead.
	ActionRedirected Action = "redirected"
	// ActionDryRun means the message is rendered and logged but never sent.
	ActionDryRun Action = "dry_run"
	// ActionFounderExisting means the registrant already holds a non-expiring
	// founder badge, so this later-season registration mails nothing.
	ActionFounderExisting Action = "skipped_founder_existing"
)

// Decision is the guard's verdict for one intended recipient.
type Decision struct {
	// Action is what happened, for logs and the dedupe record.
	Action Action
	// Mode is the guard mode that produced this decision.
	Mode config.EmailMode
	// IntendedRecipient is the registrant's real address.
	IntendedRecipient string
	// Recipients are the envelope recipients. Empty when nothing is sent.
	Recipients []string
	// SubjectPrefix is prepended to the subject in redirect mode.
	SubjectPrefix string
	// BodyNotice is injected into the email header block in redirect mode.
	BodyNotice string
}

// Send reports whether an SMTP transaction should occur.
func (d Decision) Send() bool { return len(d.Recipients) > 0 }

// Guard decides, per recipient, whether outbound mail actually goes out.
type Guard struct {
	mode       config.EmailMode
	allowlist  map[string]struct{}
	redirectTo string
}

// NewGuard builds a Guard from configuration, re-checking the fail-fast rules so
// a Guard can never exist in an inconsistent state even if built directly.
func NewGuard(mode config.EmailMode, allowlist []string, redirectTo string) (Guard, error) {
	g := Guard{mode: mode, redirectTo: config.NormalizeAddress(redirectTo), allowlist: map[string]struct{}{}}
	for _, addr := range allowlist {
		if norm := config.NormalizeAddress(addr); norm != "" {
			g.allowlist[norm] = struct{}{}
		}
	}

	switch mode {
	case config.ModeLive, config.ModeDryRun, "": // unset means live (spec §4)
	case config.ModeAllowlist:
		if len(g.allowlist) == 0 {
			return Guard{}, errors.New("mailer: allowlist mode requires a non-empty allowlist")
		}
	case config.ModeRedirect:
		if g.redirectTo == "" {
			return Guard{}, errors.New("mailer: redirect mode requires a redirect address")
		}
	default:
		return Guard{}, fmt.Errorf("mailer: unknown email mode %q", mode)
	}
	return g, nil
}

// GuardFromConfig builds the Guard described by cfg.
func GuardFromConfig(cfg *config.Config) (Guard, error) {
	return NewGuard(cfg.EmailMode, cfg.Allowlist, cfg.RedirectTo)
}

// Mode returns the guard's mode, used by /healthz.
func (g Guard) Mode() config.EmailMode {
	if g.mode == "" {
		return config.ModeLive
	}
	return g.mode
}

// Decide evaluates one intended recipient.
func (g Guard) Decide(recipient string) Decision {
	addr := config.NormalizeAddress(recipient)
	d := Decision{Mode: g.Mode(), IntendedRecipient: addr}

	switch g.Mode() {
	case config.ModeDryRun:
		d.Action = ActionDryRun
	case config.ModeAllowlist:
		if _, ok := g.allowlist[addr]; ok {
			d.Action = ActionSent
			d.Recipients = []string{addr}
		} else {
			d.Action = ActionSkipped
		}
	case config.ModeRedirect:
		d.Action = ActionRedirected
		d.Recipients = []string{g.redirectTo}
		d.SubjectPrefix = fmt.Sprintf("[would-send: %s] ", addr)
		d.BodyNotice = fmt.Sprintf("Delivery guard: redirect mode. This message would have been sent to %s.", addr)
	default: // live
		d.Action = ActionSent
		d.Recipients = []string{addr}
	}
	return d
}
