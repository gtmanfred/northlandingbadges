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
//	Day Pass          -> purchase date + 1 day, at 23:59:59 club-local
//	Season Membership -> December 31 of the purchase year, at 23:59:59 club-local
//
// The purchase instant is first converted into loc, so the calendar date used is
// the club's date rather than UTC's. Arithmetic is done on calendar fields and
// the wall clock is then pinned to 23:59:59, which keeps the result correct
// across DST transitions: the badge always dies at local midnight-minus-a-second
// regardless of how many hours that day actually had.
func Calculate(passType domain.PassType, purchasedAt time.Time, loc *time.Location) (time.Time, error) {
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
	case domain.PassTypeSeason:
		return time.Date(year, time.December, 31, 23, 59, 59, 0, loc), nil
	default:
		return time.Time{}, fmt.Errorf("expiry: %w: %q", domain.ErrUnknownPassType, passType)
	}
}
