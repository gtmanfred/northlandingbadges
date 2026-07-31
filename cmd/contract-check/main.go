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
	"os"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/dgs"
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
		return errors.New("DGS_EVENT_SLUG, DGS_SEASON_YEAR, DGS_EMAIL and DGS_PASSWORD are required")
	}

	client, err := dgs.NewExportClient(cfg, loc, log)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

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
