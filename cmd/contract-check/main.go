// Command contract-check verifies that the DiscGolfScene parser still extracts
// the fields it needs from the live club page.
//
// DiscGolfScene publishes no API contract, so this runs on a daily schedule
// outside the merge gate: a failure means upstream markup changed, not that this
// repo regressed. The workflow turns a non-zero exit into a GitHub issue
// (spec §6, contract drift).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "time/tzdata"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/dgs"
	"github.com/northlanding/badges/internal/domain"
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
	rosterURL := os.Getenv("DGS_ROSTER_URL")
	if rosterURL == "" {
		return fmt.Errorf("DGS_ROSTER_URL is required")
	}
	tz := os.Getenv("CLUB_TIMEZONE")
	if tz == "" {
		tz = config.DefaultTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("CLUB_TIMEZONE: %w", err)
	}

	client, err := dgs.NewClient(config.DGSConfig{
		RosterURL: rosterURL,
		LoginURL:  os.Getenv("DGS_LOGIN_URL"),
		Username:  os.Getenv("DGS_USERNAME"),
		Password:  os.Getenv("DGS_PASSWORD"),
	}, loc, log)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	registrations, errs := client.Fetch(ctx)
	for _, err := range errs {
		log.Warn("row or fetch warning", "error", err)
	}
	if len(registrations) == 0 {
		return fmt.Errorf("parser extracted zero registrations from %s (warnings: %v)", rosterURL, errs)
	}

	// Every parsed row must carry the fields the pipeline needs, and at least one
	// must classify: a page full of unclassifiable items means the item labels
	// changed.
	var classified int
	for _, reg := range registrations {
		if err := reg.Validate(); err != nil {
			return fmt.Errorf("parsed row is unusable: %w", err)
		}
		if _, err := domain.ClassifyPassType(reg.RawPassType); err == nil {
			classified++
		}
	}
	if classified == 0 {
		return fmt.Errorf("none of the %d parsed rows classify as a Day Pass or Season Membership; item labels likely changed", len(registrations))
	}

	log.Info("parser output looks healthy",
		"registrations", len(registrations), "classified", classified, "row_warnings", len(errs))
	return nil
}
