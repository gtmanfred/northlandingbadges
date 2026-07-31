package dgs

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/northlanding/badges/internal/domain"
)

// ClassifyDivision maps a DiscGolfScene season-membership division code onto a
// PassType. The division, not a free-text label, is what the admin export
// carries, and each division is its own badge tier: MEM members and SPON
// sponsors expire with the season, FNDR founders never do.
//
// There is deliberately no fallback to a plain season membership: an
// unrecognised division means DiscGolfScene changed its divisions, and minting a
// member badge silently is worse than raising an admin exception (spec §6).
func ClassifyDivision(division string) (domain.PassType, error) {
	switch normalizeDivision(division) {
	case "mem":
		return domain.PassTypeSeason, nil
	case "fndr":
		return domain.PassTypeFounder, nil
	case "spon":
		return domain.PassTypeSponsor, nil
	}
	return "", fmt.Errorf("dgs: %w: division %q", domain.ErrUnknownPassType, division)
}

// IsTotalsRow reports whether a row is the export's trailing summary line. It
// carries a fee total instead of a registrant and must be dropped before
// classification rather than reported as an unknown division.
func IsTotalsRow(division string) bool { return normalizeDivision(division) == "totals" }

// seasonYearPattern matches a four-digit season year in an event label or slug,
// e.g. "2026 Season Membership" or "…Membership_2026_Season".
var seasonYearPattern = regexp.MustCompile(`\b(20\d{2})\b`)

// SeasonYearFromLabel extracts the membership season year from a
// DiscGolfScene event label or slug, reporting false when the label carries no
// year.
//
// Season expiry cannot be derived from the purchase date — 2026-season
// registrations open in November 2025 — so a label without a year leaves the
// season year unset and the registration is rejected downstream rather than
// silently expiring a year early.
func SeasonYearFromLabel(label string) (int, bool) {
	match := seasonYearPattern.FindStringSubmatch(strings.ReplaceAll(label, "_", " "))
	if match == nil {
		return 0, false
	}
	year, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	return year, true
}

func normalizeDivision(division string) string {
	return strings.ToLower(strings.Join(strings.Fields(division), " "))
}
