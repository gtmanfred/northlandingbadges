package dgs_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/dgs"
	"github.com/northlanding/badges/internal/domain"
)

const slug = "North_Landing_Disc_Golf_Membership_2026_Season"

// clubTZ is defined in parse_test.go and shared across this package's tests.

func TestRegistrationIDIsStableAndCaseInsensitive(t *testing.T) {
	t.Parallel()
	a := dgs.RegistrationID(slug, "Casey@Example.com")
	b := dgs.RegistrationID(slug, "  casey@example.com ")
	if a != b {
		t.Errorf("ID differs by case/whitespace: %q vs %q", a, b)
	}
	if len(a) != 12 {
		t.Errorf("ID = %q, want 12 characters", a)
	}
	if other := dgs.RegistrationID("Other_Event", "casey@example.com"); other == a {
		t.Error("different event slugs must produce different IDs")
	}
	if blank := dgs.RegistrationID(slug, "  "); blank != "" {
		t.Errorf("ID for a blank email = %q, want empty", blank)
	}
}

func TestParseExportFixture(t *testing.T) {
	t.Parallel()
	f, err := os.Open("testdata/registrations.csv")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	ny := clubTZ(t)
	candidates, errs := dgs.ParseExport(f, slug, 2026, ny)

	// 4 good rows: two MEM, one FNDR, one SPON. Skipped: Totals (silently),
	// blank email, unknown division PRO, unparseable date.
	if len(candidates) != 4 {
		t.Fatalf("parsed %d candidates, want 4: %+v", len(candidates), candidates)
	}
	if len(errs) != 3 {
		t.Fatalf("got %d row errors, want 3: %v", len(errs), errs)
	}

	byEmail := map[string]domain.Candidate{}
	for _, c := range candidates {
		byEmail[strings.ToLower(c.Registration.Email)] = c
	}

	casey, ok := byEmail["casey@example.com"]
	if !ok {
		t.Fatal("casey@example.com missing from candidates")
	}
	if casey.PassType != domain.PassTypeSeason {
		t.Errorf("casey pass type = %q, want %q", casey.PassType, domain.PassTypeSeason)
	}
	if casey.Registration.SeasonYear != 2026 {
		t.Errorf("casey season year = %d, want 2026", casey.Registration.SeasonYear)
	}
	if casey.Registration.ID != dgs.RegistrationID(slug, "casey@example.com") {
		t.Errorf("casey ID = %q, want the derived hash", casey.Registration.ID)
	}
	if casey.Registration.Name != "Casey Chains" {
		t.Errorf("casey name = %q", casey.Registration.Name)
	}
	want := time.Date(2025, 11, 13, 1, 7, 27, 0, ny)
	if !casey.Registration.PurchasedAt.Equal(want) {
		t.Errorf("casey purchased at %s, want %s", casey.Registration.PurchasedAt, want)
	}
	if err := casey.Registration.Validate(); err != nil {
		t.Errorf("parsed row is unusable: %v", err)
	}

	if got := byEmail["dana@example.com"].PassType; got != domain.PassTypeFounder {
		t.Errorf("dana pass type = %q, want %q", got, domain.PassTypeFounder)
	}
	if got := byEmail["sam@example.com"].PassType; got != domain.PassTypeSponsor {
		t.Errorf("sam pass type = %q, want %q", got, domain.PassTypeSponsor)
	}

	// Each of the three bad rows must fail for its own reason, not just contribute
	// to the total count: a regression that miscounted the blank-email row as a
	// date error, say, would still pass a bare len(errs) == 3 check.
	unclaimed := make([]error, len(errs))
	copy(unclaimed, errs)
	claim := func(match func(error) bool) error {
		for i, err := range unclaimed {
			if err != nil && match(err) {
				unclaimed[i] = nil
				return err
			}
		}
		return nil
	}

	if err := claim(func(err error) bool { return errors.Is(err, domain.ErrUnknownPassType) }); err == nil {
		t.Errorf("expected an ErrUnknownPassType row error for division PRO, got %v", errs)
	}
	if err := claim(func(err error) bool {
		return strings.Contains(err.Error(), "missing email") && strings.Contains(err.Error(), "Morgan Mando")
	}); err == nil {
		t.Errorf("expected a missing-email row error naming Morgan Mando, got %v", errs)
	}
	if err := claim(func(err error) bool {
		return strings.Contains(err.Error(), "unrecognized date") && strings.Contains(err.Error(), "not a date")
	}); err == nil {
		t.Errorf("expected an unrecognized-date row error for the bad-date row, got %v", errs)
	}
}

func TestParseExportRejectsMissingColumn(t *testing.T) {
	t.Parallel()
	// No Email column.
	csv := "Division,Name,Registration date EST\nMEM,Casey Chains,2026-04-01 08:02:11\n"
	_, errs := dgs.ParseExport(strings.NewReader(csv), slug, 2026, time.UTC)
	if len(errs) != 1 || !errors.Is(errs[0], dgs.ErrMissingColumn) {
		t.Fatalf("errs = %v, want one ErrMissingColumn", errs)
	}
}

func TestParseExportRejectsMissingSeasonYear(t *testing.T) {
	t.Parallel()
	csv := "Division,Name,Email,Registration date EST\nMEM,Casey Chains,casey@example.com,2026-04-01 08:02:11\n"
	if _, errs := dgs.ParseExport(strings.NewReader(csv), slug, 0, time.UTC); len(errs) == 0 {
		t.Fatal("ParseExport with seasonYear 0 returned no error")
	}
}

func TestParseExportEmptyBody(t *testing.T) {
	t.Parallel()
	if _, errs := dgs.ParseExport(strings.NewReader(""), slug, 2026, time.UTC); len(errs) == 0 {
		t.Fatal("ParseExport on an empty body returned no error")
	}
}

func TestParseExportReadsPDGANumber(t *testing.T) {
	t.Parallel()
	csv := "Division,Name,Email,PDGA#,Registration date EST\n" +
		"MEM,Casey Chains,casey@example.com,12345,11/13/2025\n"
	got, errs := dgs.ParseExport(strings.NewReader(csv), "event-2026", 2026, time.UTC)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].Registration.PDGANumber != "12345" {
		t.Errorf("PDGANumber = %q, want \"12345\"", got[0].Registration.PDGANumber)
	}
}

func TestParseExportWithoutPDGAColumn(t *testing.T) {
	t.Parallel()
	// A club export lacking the column must still produce badges.
	csv := "Division,Name,Email,Registration date EST\n" +
		"MEM,Casey Chains,casey@example.com,11/13/2025\n"
	got, errs := dgs.ParseExport(strings.NewReader(csv), "event-2026", 2026, time.UTC)
	for _, err := range errs {
		if errors.Is(err, dgs.ErrMissingColumn) {
			t.Fatalf("a missing PDGA# column must not be ErrMissingColumn: %v", err)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].Registration.PDGANumber != "" {
		t.Errorf("PDGANumber = %q, want empty", got[0].Registration.PDGANumber)
	}
}

func TestParseExportRejectsNonNumericPDGAButKeepsTheRow(t *testing.T) {
	t.Parallel()
	csv := "Division,Name,Email,PDGA#,Registration date EST\n" +
		"MEM,Casey Chains,casey@example.com,pending,11/13/2025\n"
	got, errs := dgs.ParseExport(strings.NewReader(csv), "event-2026", 2026, time.UTC)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1 — a bad PDGA number must not cost a badge", len(got))
	}
	if got[0].Registration.PDGANumber != "" {
		t.Errorf("PDGANumber = %q, want empty for a non-numeric value", got[0].Registration.PDGANumber)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one warning", errs)
	}
	if !strings.Contains(errs[0].Error(), "pending") {
		t.Errorf("warning %q should name the offending value", errs[0].Error())
	}
}
