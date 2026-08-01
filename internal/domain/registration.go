// Package domain holds the core types shared across the badge pipeline.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// PassType is the kind of pass a registrant bought.
type PassType string

const (
	// PassTypeDay is a single Day Pass.
	PassTypeDay PassType = "day_pass"
	// PassTypeSeason is a season membership for a calendar year.
	PassTypeSeason PassType = "season_membership"
	// PassTypeFounder is a Course Founder membership. Founder badges never
	// expire, so their Badge.ExpiresAt is the zero time.
	PassTypeFounder PassType = "founder"
	// PassTypeSponsor is a Course Sponsor membership. It expires with the season,
	// exactly like a season membership.
	PassTypeSponsor PassType = "sponsor"
)

// ErrUnknownPassType is returned when a registration's pass label cannot be
// classified. Callers must surface it: an unclassifiable registration is never
// silently treated as a Day Pass.
var ErrUnknownPassType = errors.New("unknown pass type")

// Label is the human-readable name used on badges, passes and email copy.
func (p PassType) Label() string {
	switch p {
	case PassTypeDay:
		return "Day Pass"
	case PassTypeSeason:
		return "Season Member"
	case PassTypeFounder:
		return "Founder"
	case PassTypeSponsor:
		return "Course Sponsor"
	default:
		return string(p)
	}
}

// Registration is one purchase pulled from DiscGolfScene.
type Registration struct {
	// ID is derived as hex(sha256(eventSlug + "|" + lower(trim(email))))[:12] by
	// the ingest layer, because the DiscGolfScene export carries no
	// registration ID of its own. It is the dedupe key and the wallet pass
	// serial number.
	ID string
	// Name is the guest name shown on the badge.
	Name string
	// Email is the registrant's address.
	Email string
	// RawPassType is the label exactly as DiscGolfScene reported it.
	RawPassType string
	// PurchasedAt is the purchase instant; expiration is derived from its
	// calendar date in the club's timezone.
	PurchasedAt time.Time
	// SeasonYear is the membership season the registration belongs to, taken from
	// the DiscGolfScene event rather than the purchase date: 2026-season
	// registrations begin in November 2025, so the purchase year is wrong. Zero
	// for day passes, which expire relative to the purchase date.
	SeasonYear int
}

// Validate reports whether the registration carries the fields the pipeline
// needs.
func (r Registration) Validate() error {
	switch {
	case strings.TrimSpace(r.ID) == "":
		return errors.New("registration: missing id")
	case strings.TrimSpace(r.Name) == "":
		return fmt.Errorf("registration %s: missing name", r.ID)
	case strings.TrimSpace(r.Email) == "":
		return fmt.Errorf("registration %s: missing email", r.ID)
	case r.PurchasedAt.IsZero():
		return fmt.Errorf("registration %s: missing purchase date", r.ID)
	}
	return nil
}

// ClassifyPassType maps a DiscGolfScene pass label onto a PassType.
//
// Matching is case- and whitespace-insensitive. Anything unrecognised returns
// ErrUnknownPassType rather than a default, per spec §6.
func ClassifyPassType(raw string) (PassType, error) {
	norm := strings.ToLower(strings.Join(strings.Fields(raw), " "))
	if norm == "" {
		return "", fmt.Errorf("%w: %q", ErrUnknownPassType, raw)
	}
	switch {
	case strings.Contains(norm, "day pass"), strings.Contains(norm, "daypass"), norm == "day":
		return PassTypeDay, nil
	case strings.Contains(norm, "season"), strings.Contains(norm, "annual membership"):
		return PassTypeSeason, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownPassType, raw)
}

// Candidate is a registration whose pass type the ingest layer already resolved.
// DiscGolfScene reports a division code, not a pass label, so classification
// happens where the division is read rather than downstream in the pipeline.
type Candidate struct {
	Registration Registration
	PassType     PassType
}

// Badge is a fully resolved pass ready to be rendered, signed and mailed.
type Badge struct {
	Registration Registration
	PassType     PassType
	ExpiresAt    time.Time
}

// Expires reports whether the badge has an expiration at all. Founder badges do
// not, and a zero ExpiresAt must never be formatted as a real date.
func (b Badge) Expires() bool { return !b.ExpiresAt.IsZero() }
