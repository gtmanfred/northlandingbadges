// Package poll runs one ingestion cycle: fetch registrations, classify them,
// generate badge artwork and wallet passes, apply the delivery guard, and record
// the result so nobody is mailed twice.
package poll

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/northlanding/badges/internal/badge"
	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/email"
	"github.com/northlanding/badges/internal/expiry"
	"github.com/northlanding/badges/internal/mailer"
	"github.com/northlanding/badges/internal/store"
)

// Fetcher pulls candidate registrations from DiscGolfScene.
type Fetcher interface {
	Fetch(ctx context.Context) ([]domain.Registration, []error)
}

// Deliverer renders and conditionally sends the badge email.
type Deliverer interface {
	Deliver(ctx context.Context, registrationID string, data email.Data) (mailer.Result, error)
}

// ApplePassBuilder signs a .pkpass bundle.
type ApplePassBuilder interface {
	Build(b domain.Badge, loc *time.Location) ([]byte, error)
}

// GooglePassBuilder mints a Save-to-Google-Wallet link.
type GooglePassBuilder interface {
	SaveLink(b domain.Badge, loc *time.Location) (string, error)
}

// Service owns one poll cycle.
type Service struct {
	Fetcher  Fetcher
	Store    *store.Store
	Mailer   Deliverer
	Apple    ApplePassBuilder
	Google   GooglePassBuilder
	Location *time.Location
	// BaseURL is the public origin used to build the Apple Wallet download link.
	BaseURL string
	Log     *slog.Logger
}

// Report summarizes a cycle. It is returned as the poll endpoint's JSON body so
// the GitHub Actions log shows what happened.
type Report struct {
	Fetched        int      `json:"fetched"`
	Processed      int      `json:"processed"`
	AlreadySeen    int      `json:"already_seen"`
	Sent           int      `json:"sent"`
	SkippedGuard   int      `json:"skipped_by_guard"`
	Redirected     int      `json:"redirected"`
	DryRun         int      `json:"dry_run"`
	Unclassified   int      `json:"unclassified"`
	SkippedFounder int      `json:"skipped_founder_existing"`
	Failed         int      `json:"failed"`
	IngestWarning  []string `json:"ingest_warnings,omitempty"`
	Errors         []string `json:"errors,omitempty"`
}

// Outcome is the result of processing a single registration.
type Outcome struct {
	RegistrationID string
	Duplicate      bool
	Action         mailer.Action
	ExpiresAt      time.Time
	PassType       domain.PassType
}

// RunCycle processes every registration the fetcher returns.
//
// The cycle is idempotent: registrations already in the ledger are skipped, so a
// retried or duplicated trigger is a no-op, and a cycle that finds nothing new
// does no work (spec §4).
func (s *Service) RunCycle(ctx context.Context) (Report, error) {
	if s.Fetcher == nil {
		return Report{}, errors.New("poll: no fetcher configured")
	}
	log := s.logger()

	registrations, ingestErrs := s.Fetcher.Fetch(ctx)
	report := Report{Fetched: len(registrations)}
	for _, err := range ingestErrs {
		report.IngestWarning = append(report.IngestWarning, err.Error())
		log.Warn("registration ingest warning", "error", err)
	}
	// A fetch that yielded nothing but produced errors is a real failure, not an
	// empty roster.
	if len(registrations) == 0 && len(ingestErrs) > 0 {
		return report, fmt.Errorf("poll: ingest failed: %w", errors.Join(ingestErrs...))
	}

	for _, reg := range registrations {
		if err := ctx.Err(); err != nil {
			report.Errors = append(report.Errors, err.Error())
			return report, err
		}
		outcome, err := s.Process(ctx, reg)
		switch {
		case errors.Is(err, domain.ErrUnknownPassType):
			report.Unclassified++
			report.Errors = append(report.Errors, err.Error())
			log.Warn("registration skipped: unknown pass type",
				"registration_id", reg.ID, "raw_pass_type", reg.RawPassType)
			continue
		case err != nil:
			report.Failed++
			report.Errors = append(report.Errors, err.Error())
			log.Error("registration failed", "registration_id", reg.ID, "error", err)
			continue
		}

		if outcome.Duplicate {
			report.AlreadySeen++
			continue
		}
		report.Processed++
		switch outcome.Action {
		case mailer.ActionSent:
			report.Sent++
		case mailer.ActionSkipped:
			report.SkippedGuard++
		case mailer.ActionRedirected:
			report.Redirected++
		case mailer.ActionDryRun:
			report.DryRun++
		case mailer.ActionFounderExisting:
			report.SkippedFounder++
		}
	}

	log.Info("poll cycle complete",
		"fetched", report.Fetched, "processed", report.Processed, "already_seen", report.AlreadySeen,
		"sent", report.Sent, "skipped_by_guard", report.SkippedGuard, "redirected", report.Redirected,
		"dry_run", report.DryRun, "unclassified", report.Unclassified,
		"skipped_founder_existing", report.SkippedFounder, "failed", report.Failed)
	return report, nil
}

// Process handles one registration end to end. It is also the webhook path, so
// both ingestion options share exactly one pipeline.
func (s *Service) Process(ctx context.Context, reg domain.Registration) (Outcome, error) {
	if err := reg.Validate(); err != nil {
		return Outcome{}, fmt.Errorf("poll: %w", err)
	}
	passType, err := domain.ClassifyPassType(reg.RawPassType)
	if err != nil {
		return Outcome{}, fmt.Errorf("poll: registration %s: %w", reg.ID, err)
	}
	return s.ProcessClassified(ctx, reg, passType)
}

// ProcessClassified handles a registration whose pass type is already known.
func (s *Service) ProcessClassified(ctx context.Context, reg domain.Registration, passType domain.PassType) (Outcome, error) {
	if s.Store == nil {
		return Outcome{}, errors.New("poll: no store configured")
	}
	if s.Mailer == nil {
		return Outcome{}, errors.New("poll: no mailer configured")
	}
	log := s.logger()

	// Claim first: whoever wins the insert owns the delivery. This is what makes
	// concurrent duplicates produce exactly one email.
	claimed, err := s.Store.Claim(ctx, reg.ID)
	if err != nil {
		return Outcome{}, err
	}
	if !claimed {
		log.Info("registration already processed", "registration_id", reg.ID)
		return Outcome{RegistrationID: reg.ID, Duplicate: true, PassType: passType}, nil
	}

	outcome, err := s.build(ctx, reg, passType)
	if err != nil {
		// Nothing was delivered, so release the claim to allow a later retry.
		if relErr := s.Store.Release(ctx, reg.ID); relErr != nil {
			log.Error("failed to release claim after error", "registration_id", reg.ID, "error", relErr)
		}
		return Outcome{}, err
	}
	return outcome, nil
}

func (s *Service) build(ctx context.Context, reg domain.Registration, passType domain.PassType) (Outcome, error) {
	loc := s.location()

	// Founder badges never expire, so a founder who re-registers in a later season
	// must not receive a second one. The registration is still recorded, which
	// keeps the cycle idempotent.
	if passType == domain.PassTypeFounder {
		issued, err := s.Store.FounderIssued(ctx, reg.Email)
		if err != nil {
			return Outcome{}, err
		}
		if issued {
			if err := s.Store.MarkProcessed(ctx, store.Record{
				RegistrationID: reg.ID,
				Email:          reg.Email,
				PassType:       string(passType),
				Action:         string(mailer.ActionFounderExisting),
				ProcessedAt:    time.Now(),
			}); err != nil {
				return Outcome{}, err
			}
			s.logger().Info("founder badge already issued; nothing mailed",
				"registration_id", reg.ID, "email", reg.Email)
			return Outcome{
				RegistrationID: reg.ID,
				Action:         mailer.ActionFounderExisting,
				PassType:       passType,
			}, nil
		}
	}

	expiresAt, err := expiry.Calculate(passType, reg.PurchasedAt, reg.SeasonYear, loc)
	if err != nil {
		return Outcome{}, err
	}
	b := domain.Badge{Registration: reg, PassType: passType, ExpiresAt: expiresAt}

	artwork, err := badge.Render(b, loc)
	if err != nil {
		return Outcome{}, err
	}

	data := email.Data{Badge: b, BadgePNG: artwork, Location: loc}

	var pkpass []byte
	if s.Apple != nil {
		if pkpass, err = s.Apple.Build(b, loc); err != nil {
			return Outcome{}, err
		}
		data.PKPass = pkpass
	}
	if s.Google != nil {
		if data.GoogleURL, err = s.Google.SaveLink(b, loc); err != nil {
			return Outcome{}, err
		}
	}

	token, err := accessToken()
	if err != nil {
		return Outcome{}, err
	}
	if len(pkpass) > 0 {
		data.AppleURL = s.applePassURL(reg.ID, token)
	}
	if len(pkpass) > 0 || data.GoogleURL != "" {
		if err := s.Store.SaveArtifact(ctx, store.Artifact{
			RegistrationID: reg.ID,
			AccessToken:    token,
			PKPass:         pkpass,
			GoogleJWT:      data.GoogleURL,
		}); err != nil {
			return Outcome{}, err
		}
	}

	result, err := s.Mailer.Deliver(ctx, reg.ID, data)
	if err != nil {
		return Outcome{}, err
	}

	// Recorded regardless of whether mail actually went out: a guard-suppressed
	// registration stays processed (spec §4).
	if err := s.Store.MarkProcessed(ctx, store.Record{
		RegistrationID: reg.ID,
		Email:          reg.Email,
		PassType:       string(passType),
		ExpiresAt:      expiresAt,
		EmailMode:      string(result.Decision.Mode),
		Action:         string(result.Decision.Action),
		ProcessedAt:    time.Now(),
	}); err != nil {
		return Outcome{}, err
	}

	return Outcome{
		RegistrationID: reg.ID,
		Action:         result.Decision.Action,
		ExpiresAt:      expiresAt,
		PassType:       passType,
	}, nil
}

// applePassURL is the link behind the "Add to Apple Wallet" button.
func (s *Service) applePassURL(registrationID, token string) string {
	base := s.BaseURL
	if base == "" {
		base = "http://localhost:8080"
	}
	return fmt.Sprintf("%s/passes/%s.pkpass?t=%s", base, url.PathEscape(registrationID), url.QueryEscape(token))
}

func (s *Service) location() *time.Location {
	if s.Location == nil {
		return time.UTC
	}
	return s.Location
}

func (s *Service) logger() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// accessToken is the unguessable component of the pass download URL. The link is
// emailed, so it acts as a bearer capability for that one pass.
func accessToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("poll: generate access token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
