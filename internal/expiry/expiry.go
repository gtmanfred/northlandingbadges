// Package expiry derives badge expiration timestamps from purchase data.
package expiry

import (
	"errors"
	"fmt"
	"time"

	"github.com/northlanding/badges/internal/domain"
)

// Calculate returns the badge expiration for a purchase, per spec §4:
//
//	Day Pass                    -> purchase date + 1 day, at 23:59:59 club-local
//	Season Membership / Sponsor -> December 31 of seasonYear, at 23:59:59 club-local
//	Founder                     -> no expiration (the zero time)
//
// seasonYear comes from the DiscGolfScene event, not from purchasedAt: a 2026
// season membership can be bought in November 2025, and deriving the year from
// the purchase would expire that badge before its season began.
//
// The purchase instant is first converted into loc, so the calendar date used is
// the club's date rather than UTC's. Arithmetic is done on calendar fields and
// the wall clock is then pinned to 23:59:59, which keeps the result correct
// across DST transitions: the badge always dies at local midnight-minus-a-second
// regardless of how many hours that day actually had.
func Calculate(passType domain.PassType, purchasedAt time.Time, seasonYear int, loc *time.Location) (time.Time, error) {
	if purchasedAt.IsZero() {
		return time.Time{}, errors.New("expiry: purchase time is zero")
	}
	if loc == nil {
		loc = time.UTC
	}

	local := purchasedAt.In(loc)
	year, month, day := local.Date()

	switch passType {
	case domain.PassTypeDay:
		// time.Date normalizes overflow, so day+1 rolls month and year over and
		// respects leap years without special cases.
		return time.Date(year, month, day+1, 23, 59, 59, 0, loc), nil
	case domain.PassTypeSeason, domain.PassTypeSponsor:
		if seasonYear == 0 {
			return time.Time{}, fmt.Errorf("expiry: %q needs a season year", passType)
		}
		return time.Date(seasonYear, time.December, 31, 23, 59, 59, 0, loc), nil
	case domain.PassTypeFounder:
		// Zero time means "never expires"; callers test Badge.Expires().
		return time.Time{}, nil
	default:
		return time.Time{}, fmt.Errorf("expiry: %w: %q", domain.ErrUnknownPassType, passType)
	}
}
