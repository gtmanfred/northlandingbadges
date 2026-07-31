package config_test

import (
	"strings"
	"testing"

	"github.com/northlanding/badges/internal/config"
)

// env builds a Getenv over a map, starting from a minimal valid environment.
func env(overrides map[string]string) config.Getenv {
	base := map[string]string{
		"POLL_TRIGGER_TOKEN": "s3cret",
		"GMAIL_USER":         "club@gmail.com",
		"GMAIL_APP_PASSWORD": "abcd efgh ijkl mnop",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(key string) string { return base[key] }
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmailMode != config.ModeLive {
		t.Errorf("EmailMode = %q, want live (default when unset)", cfg.EmailMode)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.DBPath != "/data/badges.db" {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}
	if cfg.SMTPAddr != config.DefaultSMTPAddr {
		t.Errorf("SMTPAddr = %q", cfg.SMTPAddr)
	}
	if cfg.ClubTimezone == nil || cfg.ClubTimezone.String() != config.DefaultTimezone {
		t.Errorf("ClubTimezone = %v, want %s", cfg.ClubTimezone, config.DefaultTimezone)
	}
	if !cfg.SendsMail() {
		t.Error("live mode should send mail")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(map[string]string{
		"PORT":            "3000",
		"DB_PATH":         "/tmp/x.db",
		"BASE_URL":        "https://badges.fly.dev/",
		"CLUB_TIMEZONE":   "UTC",
		"EMAIL_MODE":      "ALLOWLIST",
		"EMAIL_ALLOWLIST": " Tester@Example.com , second@example.com ,, ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":3000" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.BaseURL != "https://badges.fly.dev" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", cfg.BaseURL)
	}
	if cfg.EmailMode != config.ModeAllowlist {
		t.Errorf("EmailMode = %q, want lowercased allowlist", cfg.EmailMode)
	}
	want := []string{"tester@example.com", "second@example.com"}
	if len(cfg.Allowlist) != len(want) {
		t.Fatalf("Allowlist = %v, want %v", cfg.Allowlist, want)
	}
	for i := range want {
		if cfg.Allowlist[i] != want[i] {
			t.Fatalf("Allowlist = %v, want %v", cfg.Allowlist, want)
		}
	}
}

func TestLoadValidationFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		overrides map[string]string
		wantSub   string
	}{
		{
			name:      "allowlist mode with empty allowlist",
			overrides: map[string]string{"EMAIL_MODE": "allowlist"},
			wantSub:   "EMAIL_ALLOWLIST",
		},
		{
			name:      "allowlist mode with only separators",
			overrides: map[string]string{"EMAIL_MODE": "allowlist", "EMAIL_ALLOWLIST": " , , "},
			wantSub:   "EMAIL_ALLOWLIST",
		},
		{
			name:      "redirect mode without redirect target",
			overrides: map[string]string{"EMAIL_MODE": "redirect"},
			wantSub:   "EMAIL_REDIRECT_TO",
		},
		{
			name:      "unknown mode",
			overrides: map[string]string{"EMAIL_MODE": "silent"},
			wantSub:   "invalid",
		},
		{
			name:      "missing poll token",
			overrides: map[string]string{"POLL_TRIGGER_TOKEN": ""},
			wantSub:   "POLL_TRIGGER_TOKEN",
		},
		{
			name:      "missing gmail user in live mode",
			overrides: map[string]string{"GMAIL_USER": ""},
			wantSub:   "GMAIL_USER",
		},
		{
			name:      "missing gmail password in live mode",
			overrides: map[string]string{"GMAIL_APP_PASSWORD": ""},
			wantSub:   "GMAIL_APP_PASSWORD",
		},
		{
			name:      "bad timezone",
			overrides: map[string]string{"CLUB_TIMEZONE": "Mars/Olympus"},
			wantSub:   "CLUB_TIMEZONE",
		},
		{
			name:      "non-numeric port",
			overrides: map[string]string{"PORT": "eighty"},
			wantSub:   "PORT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(env(tc.overrides))
			if err == nil {
				t.Fatalf("expected startup failure for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestDryRunNeedsNoSMTPCredentials(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(map[string]string{
		"EMAIL_MODE":         "dry_run",
		"GMAIL_USER":         "",
		"GMAIL_APP_PASSWORD": "",
	}))
	if err != nil {
		t.Fatalf("dry_run should not require SMTP credentials: %v", err)
	}
	if cfg.SendsMail() {
		t.Error("dry_run must not send mail")
	}
}

func TestRedirectModeAcceptsTarget(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(map[string]string{
		"EMAIL_MODE":        "redirect",
		"EMAIL_REDIRECT_TO": "  QA@Example.com ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RedirectTo != "qa@example.com" {
		t.Errorf("RedirectTo = %q, want normalized", cfg.RedirectTo)
	}
}

func TestWalletConfiguredPredicates(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Apple.Configured() {
		t.Error("Apple should be unconfigured with no cert material")
	}
	if cfg.Google.Configured() {
		t.Error("Google should be unconfigured with no key material")
	}
	if cfg.DGS.Configured() {
		t.Error("DGS should be unconfigured with no roster URL")
	}

	full, err := config.Load(env(map[string]string{
		"APPLE_PASS_TYPE_ID": "pass.com.northlanding.badge",
		"APPLE_TEAM_ID":      "TEAM123",
		"APPLE_CERT_PEM":     "-----BEGIN CERTIFICATE-----",
		"APPLE_KEY_PEM":      "-----BEGIN PRIVATE KEY-----",
		"GOOGLE_ISSUER_ID":   "3388000000000000000",
		"GOOGLE_CLASS_ID":    "3388000000000000000.northlanding",
		"GOOGLE_SA_EMAIL":    "wallet@proj.iam.gserviceaccount.com",
		"GOOGLE_SA_KEY_PEM":  "-----BEGIN PRIVATE KEY-----",
		"DGS_EVENT_SLUG":     "North_Landing_Disc_Golf_Membership_2026_Season",
		"DGS_SEASON_YEAR":    "2026",
		"DGS_EMAIL":          "admin@example.com",
		"DGS_PASSWORD":       "secret",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !full.Apple.Configured() || !full.Google.Configured() || !full.DGS.Configured() {
		t.Error("expected all integrations configured")
	}
}

func TestLoadRejectsNilGetenv(t *testing.T) {
	t.Parallel()
	if _, err := config.Load(nil); err == nil {
		t.Fatal("expected error for nil getenv")
	}
}

func TestLoadDGSExportConfig(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(map[string]string{
		"POLL_TRIGGER_TOKEN": "t",
		"EMAIL_MODE":         "dry_run",
		"DGS_EVENT_SLUG":     "North_Landing_Disc_Golf_Membership_2026_Season",
		"DGS_SEASON_YEAR":    "2026",
		"DGS_EMAIL":          "admin@example.com",
		"DGS_PASSWORD":       "secret",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.DGS.Configured() {
		t.Fatal("DGS should be configured")
	}
	if cfg.DGS.SeasonYear != 2026 {
		t.Errorf("SeasonYear = %d, want 2026", cfg.DGS.SeasonYear)
	}
	if got, want := cfg.DGS.LoginURL(), "https://www.discgolfscene.com/auth/sign-in"; got != want {
		t.Errorf("LoginURL = %q, want %q", got, want)
	}
	want := "https://www.discgolfscene.com/tournaments/North_Landing_Disc_Golf_Membership_2026_Season/admin/registration-export"
	if got := cfg.DGS.ExportURL(); got != want {
		t.Errorf("ExportURL = %q, want %q", got, want)
	}
}

func TestLoadDGSBaseURLOverrideAndTrailingSlash(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(map[string]string{
		"POLL_TRIGGER_TOKEN": "t",
		"EMAIL_MODE":         "dry_run",
		"DGS_BASE_URL":       "http://127.0.0.1:9999/",
		"DGS_EVENT_SLUG":     "Slug",
		"DGS_SEASON_YEAR":    "2026",
		"DGS_EMAIL":          "admin@example.com",
		"DGS_PASSWORD":       "secret",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.DGS.LoginURL(), "http://127.0.0.1:9999/auth/sign-in"; got != want {
		t.Errorf("LoginURL = %q, want %q", got, want)
	}
}

func TestLoadDGSPartialConfigIsNotConfigured(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(map[string]string{
		"POLL_TRIGGER_TOKEN": "t",
		"EMAIL_MODE":         "dry_run",
		"DGS_EVENT_SLUG":     "Slug",
		// no season year, email or password
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DGS.Configured() {
		t.Error("DGS with only a slug should not be Configured()")
	}
}

func TestLoadRejectsNonNumericSeasonYear(t *testing.T) {
	t.Parallel()
	_, err := config.Load(env(map[string]string{
		"POLL_TRIGGER_TOKEN": "t",
		"EMAIL_MODE":         "dry_run",
		"DGS_SEASON_YEAR":    "twenty twenty six",
	}))
	if err == nil {
		t.Fatal("Load with a non-numeric DGS_SEASON_YEAR = nil error, want an error")
	}
}
