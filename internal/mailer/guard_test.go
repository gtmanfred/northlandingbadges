package mailer_test

import (
	"strings"
	"testing"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/mailer"
)

func TestGuardDecide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mode       config.EmailMode
		allowlist  []string
		redirectTo string
		recipient  string
		wantAction mailer.Action
		wantTo     []string
	}{
		{
			name:       "live sends to registrant",
			mode:       config.ModeLive,
			recipient:  "Casey@Example.com",
			wantAction: mailer.ActionSent,
			wantTo:     []string{"casey@example.com"},
		},
		{
			name:       "empty mode defaults to live",
			mode:       "",
			recipient:  "casey@example.com",
			wantAction: mailer.ActionSent,
			wantTo:     []string{"casey@example.com"},
		},
		{
			name:       "allowlist match sends",
			mode:       config.ModeAllowlist,
			allowlist:  []string{"tester@example.com"},
			recipient:  "tester@example.com",
			wantAction: mailer.ActionSent,
			wantTo:     []string{"tester@example.com"},
		},
		{
			name:       "allowlist match is case-insensitive and trimmed",
			mode:       config.ModeAllowlist,
			allowlist:  []string{"  Tester@Example.COM "},
			recipient:  " TESTER@example.com ",
			wantAction: mailer.ActionSent,
			wantTo:     []string{"tester@example.com"},
		},
		{
			name:       "allowlist miss skips",
			mode:       config.ModeAllowlist,
			allowlist:  []string{"tester@example.com"},
			recipient:  "realuser@example.com",
			wantAction: mailer.ActionSkipped,
			wantTo:     nil,
		},
		{
			name:       "redirect rewrites envelope",
			mode:       config.ModeRedirect,
			redirectTo: "qa@example.com",
			recipient:  "real@user.com",
			wantAction: mailer.ActionRedirected,
			wantTo:     []string{"qa@example.com"},
		},
		{
			name:       "dry_run sends nothing",
			mode:       config.ModeDryRun,
			recipient:  "real@user.com",
			wantAction: mailer.ActionDryRun,
			wantTo:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, err := mailer.NewGuard(tc.mode, tc.allowlist, tc.redirectTo)
			if err != nil {
				t.Fatalf("NewGuard: %v", err)
			}
			got := g.Decide(tc.recipient)
			if got.Action != tc.wantAction {
				t.Errorf("Action = %q, want %q", got.Action, tc.wantAction)
			}
			if len(got.Recipients) != len(tc.wantTo) {
				t.Fatalf("Recipients = %v, want %v", got.Recipients, tc.wantTo)
			}
			for i := range tc.wantTo {
				if got.Recipients[i] != tc.wantTo[i] {
					t.Fatalf("Recipients = %v, want %v", got.Recipients, tc.wantTo)
				}
			}
			if got.Send() != (len(tc.wantTo) > 0) {
				t.Errorf("Send() = %v", got.Send())
			}
			if got.IntendedRecipient != strings.ToLower(strings.TrimSpace(tc.recipient)) {
				t.Errorf("IntendedRecipient = %q", got.IntendedRecipient)
			}
		})
	}
}

func TestRedirectDecisionAnnotatesSubjectAndBody(t *testing.T) {
	t.Parallel()
	g, err := mailer.NewGuard(config.ModeRedirect, nil, "qa@example.com")
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	d := g.Decide("real@user.com")
	if want := "[would-send: real@user.com] "; d.SubjectPrefix != want {
		t.Errorf("SubjectPrefix = %q, want %q", d.SubjectPrefix, want)
	}
	if !strings.Contains(d.BodyNotice, "real@user.com") {
		t.Errorf("BodyNotice = %q, want it to name the real recipient", d.BodyNotice)
	}
}

func TestNonRedirectDecisionsCarryNoAnnotations(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.EmailMode{config.ModeLive, config.ModeDryRun} {
		g, err := mailer.NewGuard(mode, nil, "")
		if err != nil {
			t.Fatalf("NewGuard(%q): %v", mode, err)
		}
		d := g.Decide("real@user.com")
		if d.SubjectPrefix != "" || d.BodyNotice != "" {
			t.Errorf("mode %q leaked annotations: %+v", mode, d)
		}
	}
}

func TestNewGuardRejectsInconsistentConfig(t *testing.T) {
	t.Parallel()
	if _, err := mailer.NewGuard(config.ModeAllowlist, nil, ""); err == nil {
		t.Error("allowlist mode with empty allowlist must fail")
	}
	if _, err := mailer.NewGuard(config.ModeAllowlist, []string{" ", ""}, ""); err == nil {
		t.Error("allowlist of blanks must fail")
	}
	if _, err := mailer.NewGuard(config.ModeRedirect, nil, "  "); err == nil {
		t.Error("redirect mode without target must fail")
	}
	if _, err := mailer.NewGuard(config.EmailMode("nope"), nil, ""); err == nil {
		t.Error("unknown mode must fail")
	}
}

func TestGuardFromConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{EmailMode: config.ModeAllowlist, Allowlist: []string{"tester@example.com"}}
	g, err := mailer.GuardFromConfig(cfg)
	if err != nil {
		t.Fatalf("GuardFromConfig: %v", err)
	}
	if g.Mode() != config.ModeAllowlist {
		t.Errorf("Mode = %q", g.Mode())
	}
}
