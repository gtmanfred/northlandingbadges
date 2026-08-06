// Command contract-check verifies that the DiscGolfScene club-admin CSV export
// still carries the columns and rows the pipeline needs.
//
// DiscGolfScene publishes no API contract, so this runs on a daily schedule
// outside the merge gate: a failure means upstream changed, not that this repo
// regressed. The workflow turns a non-zero exit into a GitHub issue (spec §6,
// contract drift).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/dgs"
	"github.com/northlanding/badges/internal/wallet/googlepass"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("contract check failed", "error", err)
		os.Exit(1)
	}
	log.Info("contract check passed")
}

func run(log *slog.Logger) error {
	// Each check is independent of the others, so a failure in one must not
	// stop the rest from running: the issue this run files should describe
	// everything that is broken today, not just the first thing found. Each
	// check also gets its own deadline sized to its own work, rather than
	// sharing one budget: a slow DGS export must not starve the logo check's
	// timeout and be misreported as an unreachable logo.
	dgsCtx, dgsCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer dgsCancel()

	var errs []error
	if err := checkDGSExport(dgsCtx, log); err != nil {
		errs = append(errs, err)
	}
	if err := checkLogoURI(context.Background(), &http.Client{Timeout: 15 * time.Second}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// checkDGSExport verifies that the DiscGolfScene club-admin CSV export still
// carries the columns and rows the pipeline needs.
func checkDGSExport(ctx context.Context, log *slog.Logger) error {
	tz := os.Getenv("CLUB_TIMEZONE")
	if tz == "" {
		tz = config.DefaultTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("CLUB_TIMEZONE: %w", err)
	}

	seasonYear := 0
	if raw := strings.TrimSpace(os.Getenv("DGS_SEASON_YEAR")); raw != "" {
		if seasonYear, err = strconv.Atoi(raw); err != nil {
			return fmt.Errorf("DGS_SEASON_YEAR %q is not a number", raw)
		}
	}
	cfg := config.DGSConfig{
		BaseURL:    os.Getenv("DGS_BASE_URL"),
		EventSlug:  os.Getenv("DGS_EVENT_SLUG"),
		SeasonYear: seasonYear,
		Email:      os.Getenv("DGS_EMAIL"),
		Password:   os.Getenv("DGS_PASSWORD"),
	}
	if !cfg.Configured() {
		log.Warn("skipping the DiscGolfScene contract check: DGS_EVENT_SLUG/DGS_SEASON_YEAR/DGS_EMAIL/DGS_PASSWORD are not configured")
		return nil
	}

	client, err := dgs.NewExportClient(cfg, loc, log)
	if err != nil {
		return err
	}

	candidates, errs := client.Fetch(ctx)
	for _, err := range errs {
		log.Warn("row or fetch warning", "error", err)
		// A missing column is a contract break, not a bad row.
		if errors.Is(err, dgs.ErrMissingColumn) {
			return err
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("export yielded zero usable registrations for %s (warnings: %v)", cfg.EventSlug, errs)
	}
	for _, c := range candidates {
		if err := c.Registration.Validate(); err != nil {
			return fmt.Errorf("parsed row is unusable: %w", err)
		}
		if c.PassType == "" {
			return fmt.Errorf("registration %s has no pass type", c.Registration.ID)
		}
	}

	log.Info("export output looks healthy",
		"event", cfg.EventSlug, "candidates", len(candidates), "row_warnings", len(errs))
	return nil
}

// checkLogoURI confirms Google can still fetch the club mark. A 404 here means
// every pass saved from now on renders without a logo, and Google reports
// nothing back to us when its fetch fails.
//
// It derives its own deadline from client.Timeout rather than inheriting the
// caller's context deadline, so a slow, unrelated check (e.g. the DGS export)
// cannot starve this one's budget and have a healthy logo misreported as
// unreachable.
func checkLogoURI(ctx context.Context, client *http.Client) error {
	ctx, cancel := context.WithTimeout(ctx, client.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, googlepass.LogoURI, nil)
	if err != nil {
		return fmt.Errorf("logo check: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("logo check: %s unreachable: %w", googlepass.LogoURI, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("logo check: %s returned %s, want 200", googlepass.LogoURI, resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		return fmt.Errorf("logo check: %s content-type is %q, want image/*", googlepass.LogoURI, ct)
	}
	return nil
}
