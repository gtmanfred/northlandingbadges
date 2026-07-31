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
	default:
		return string(p)
	}
}

// Registration is one purchase pulled from DiscGolfScene.
type Registration struct {
	// ID is the DiscGolfScene order/registration ID. It is the dedupe key and
	// the wallet pass serial number.
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

// Badge is a fully resolved pass ready to be rendered, signed and mailed.
type Badge struct {
	Registration Registration
	PassType     PassType
	ExpiresAt    time.Time
}
