// Package config loads and validates runtime configuration from the
// environment. Invalid combinations fail fast at startup (spec §4).
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EmailMode selects the outbound-mail delivery guard.
type EmailMode string

const (
	// ModeLive mails the real registrant. Default when EMAIL_MODE is unset.
	ModeLive EmailMode = "live"
	// ModeAllowlist mails only addresses in EMAIL_ALLOWLIST.
	ModeAllowlist EmailMode = "allowlist"
	// ModeRedirect mails everything to EMAIL_REDIRECT_TO.
	ModeRedirect EmailMode = "redirect"
	// ModeDryRun renders mail and sends nothing.
	ModeDryRun EmailMode = "dry_run"
)

// DefaultSMTPAddr is Gmail's submission endpoint (STARTTLS).
const DefaultSMTPAddr = "smtp.gmail.com:587"

// DefaultTimezone is North Landing DGC's local timezone; all expiration wall
// clocks are computed in it.
const DefaultTimezone = "America/New_York"

// Config is the fully validated runtime configuration.
type Config struct {
	Addr    string
	DBPath  string
	BaseURL string

	PollTriggerToken string
	WebhookSecret    string

	EmailMode  EmailMode
	Allowlist  []string
	RedirectTo string

	GmailUser        string
	GmailAppPassword string
	SMTPAddr         string
	FromName         string

	ClubTimezone *time.Location

	DGS    DGSConfig
	Apple  AppleConfig
	Google GoogleConfig
}

// DGSConfig addresses the DiscGolfScene fallback poller (spec §4 Option B).
type DGSConfig struct {
	RosterURL string
	LoginURL  string
	Username  string
	Password  string
}

// Configured reports whether live polling is possible.
func (d DGSConfig) Configured() bool { return d.RosterURL != "" }

// AppleConfig holds the Apple Wallet signing material.
type AppleConfig struct {
	PassTypeIdentifier string
	TeamIdentifier     string
	OrganizationName   string
	CertPEM            string
	KeyPEM             string
	KeyPassword        string
	WWDRPEM            string
}

// Configured reports whether .pkpass files can be signed.
func (a AppleConfig) Configured() bool {
	return a.PassTypeIdentifier != "" && a.TeamIdentifier != "" && a.CertPEM != "" && a.KeyPEM != ""
}

// GoogleConfig holds the Google Wallet issuer and service-account key.
type GoogleConfig struct {
	IssuerID            string
	ClassID             string
	ServiceAccountEmail string
	KeyPEM              string
}

// Configured reports whether Save-to-Google-Wallet JWTs can be minted.
func (g GoogleConfig) Configured() bool {
	return g.IssuerID != "" && g.ClassID != "" && g.ServiceAccountEmail != "" && g.KeyPEM != ""
}

// Getenv is the environment lookup, injected so tests need no process state.
type Getenv func(string) string

// Load reads configuration via getenv and validates it.
//
// Fail-fast rules (spec §4):
//   - EMAIL_MODE must be one of live|allowlist|redirect|dry_run
//   - allowlist mode requires a non-empty EMAIL_ALLOWLIST
//   - redirect mode requires EMAIL_REDIRECT_TO
//   - any mode that sends mail requires GMAIL_USER and GMAIL_APP_PASSWORD
//   - POLL_TRIGGER_TOKEN must be set, else the poll endpoint could never be used
//   - CLUB_TIMEZONE must resolve
func Load(getenv Getenv) (*Config, error) {
	if getenv == nil {
		return nil, errors.New("config: nil getenv")
	}
	get := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}

	cfg := &Config{
		Addr:             ":" + get("PORT", "8080"),
		DBPath:           get("DB_PATH", "/data/badges.db"),
		BaseURL:          strings.TrimSuffix(get("BASE_URL", "http://localhost:8080"), "/"),
		PollTriggerToken: get("POLL_TRIGGER_TOKEN", ""),
		WebhookSecret:    get("DGS_WEBHOOK_SECRET", ""),
		EmailMode:        EmailMode(strings.ToLower(get("EMAIL_MODE", string(ModeLive)))),
		Allowlist:        ParseAllowlist(getenv("EMAIL_ALLOWLIST")),
		RedirectTo:       NormalizeAddress(getenv("EMAIL_REDIRECT_TO")),
		GmailUser:        get("GMAIL_USER", ""),
		GmailAppPassword: getenv("GMAIL_APP_PASSWORD"),
		SMTPAddr:         get("SMTP_ADDR", DefaultSMTPAddr),
		FromName:         get("EMAIL_FROM_NAME", "North Landing DGC"),
		DGS: DGSConfig{
			RosterURL: get("DGS_ROSTER_URL", ""),
			LoginURL:  get("DGS_LOGIN_URL", ""),
			Username:  get("DGS_USERNAME", ""),
			Password:  getenv("DGS_PASSWORD"),
		},
		Apple: AppleConfig{
			PassTypeIdentifier: get("APPLE_PASS_TYPE_ID", ""),
			TeamIdentifier:     get("APPLE_TEAM_ID", ""),
			OrganizationName:   get("APPLE_ORG_NAME", "North Landing DGC"),
			CertPEM:            getenv("APPLE_CERT_PEM"),
			KeyPEM:             getenv("APPLE_KEY_PEM"),
			KeyPassword:        getenv("APPLE_KEY_PASSWORD"),
			WWDRPEM:            getenv("APPLE_WWDR_PEM"),
		},
		Google: GoogleConfig{
			IssuerID:            get("GOOGLE_ISSUER_ID", ""),
			ClassID:             get("GOOGLE_CLASS_ID", ""),
			ServiceAccountEmail: get("GOOGLE_SA_EMAIL", ""),
			KeyPEM:              getenv("GOOGLE_SA_KEY_PEM"),
		},
	}

	if port := getenv("PORT"); strings.TrimSpace(port) != "" {
		if _, err := strconv.Atoi(strings.TrimSpace(port)); err != nil {
			return nil, fmt.Errorf("config: PORT %q is not a number", port)
		}
	}

	loc, err := time.LoadLocation(get("CLUB_TIMEZONE", DefaultTimezone))
	if err != nil {
		return nil, fmt.Errorf("config: CLUB_TIMEZONE: %w", err)
	}
	cfg.ClubTimezone = loc

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SendsMail reports whether the configured mode can produce an SMTP send.
func (c *Config) SendsMail() bool { return c.EmailMode != ModeDryRun }

func (c *Config) validate() error {
	switch c.EmailMode {
	case ModeLive, ModeAllowlist, ModeRedirect, ModeDryRun:
	default:
		return fmt.Errorf("config: EMAIL_MODE %q is invalid (want live|allowlist|redirect|dry_run)", c.EmailMode)
	}

	if c.EmailMode == ModeAllowlist && len(c.Allowlist) == 0 {
		return errors.New("config: EMAIL_MODE=allowlist requires a non-empty EMAIL_ALLOWLIST")
	}
	if c.EmailMode == ModeRedirect && c.RedirectTo == "" {
		return errors.New("config: EMAIL_MODE=redirect requires EMAIL_REDIRECT_TO")
	}
	if c.PollTriggerToken == "" {
		return errors.New("config: POLL_TRIGGER_TOKEN is required")
	}
	if c.SendsMail() {
		if c.GmailUser == "" {
			return errors.New("config: GMAIL_USER is required unless EMAIL_MODE=dry_run")
		}
		if c.GmailAppPassword == "" {
			return errors.New("config: GMAIL_APP_PASSWORD is required unless EMAIL_MODE=dry_run")
		}
	}
	return nil
}

// ParseAllowlist splits a comma-separated allowlist, trimming whitespace and
// lowercasing so comparison is case-insensitive.
func ParseAllowlist(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if addr := NormalizeAddress(part); addr != "" {
			out = append(out, addr)
		}
	}
	return out
}

// NormalizeAddress trims and lowercases an email address for comparison.
func NormalizeAddress(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}
