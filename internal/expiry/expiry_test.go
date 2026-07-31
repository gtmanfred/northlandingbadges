package expiry_test

import (
	"errors"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/expiry"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestCalculate(t *testing.T) {
	t.Parallel()
	ny := mustLoad(t, "America/New_York")

	tests := []struct {
		name        string
		passType    domain.PassType
		purchasedAt time.Time
		want        time.Time
	}{
		{
			name:        "day pass expires next day at 23:59:59 local",
			passType:    domain.PassTypeDay,
			purchasedAt: time.Date(2026, 7, 4, 10, 15, 0, 0, ny),
			want:        time.Date(2026, 7, 5, 23, 59, 59, 0, ny),
		},
		{
			name:        "day pass bought late at night still rolls one calendar day",
			passType:    domain.PassTypeDay,
			purchasedAt: time.Date(2026, 7, 4, 23, 58, 0, 0, ny),
			want:        time.Date(2026, 7, 5, 23, 59, 59, 0, ny),
		},
		{
			name:        "day pass on new year's eve rolls into next year",
			passType:    domain.PassTypeDay,
			purchasedAt: time.Date(2026, 12, 31, 16, 0, 0, 0, ny),
			want:        time.Date(2027, 1, 1, 23, 59, 59, 0, ny),
		},
		{
			name:        "day pass before leap day lands on feb 29",
			passType:    domain.PassTypeDay,
			purchasedAt: time.Date(2028, 2, 28, 9, 0, 0, 0, ny),
			want:        time.Date(2028, 2, 29, 23, 59, 59, 0, ny),
		},
		{
			name:        "day pass on leap day lands on march 1",
			passType:    domain.PassTypeDay,
			purchasedAt: time.Date(2028, 2, 29, 9, 0, 0, 0, ny),
			want:        time.Date(2028, 3, 1, 23, 59, 59, 0, ny),
		},
		{
			name:        "day pass across spring-forward DST keeps 23:59:59 local",
			passType:    domain.PassTypeDay,
			purchasedAt: time.Date(2026, 3, 7, 20, 0, 0, 0, ny),
			want:        time.Date(2026, 3, 8, 23, 59, 59, 0, ny),
		},
		{
			name:        "day pass across fall-back DST keeps 23:59:59 local",
			passType:    domain.PassTypeDay,
			purchasedAt: time.Date(2026, 10, 31, 20, 0, 0, 0, ny),
			want:        time.Date(2026, 11, 1, 23, 59, 59, 0, ny),
		},
		{
			name:        "utc purchase is interpreted in club timezone",
			passType:    domain.PassTypeDay,
			purchasedAt: time.Date(2026, 7, 5, 2, 0, 0, 0, time.UTC), // 2026-07-04 22:00 ET
			want:        time.Date(2026, 7, 5, 23, 59, 59, 0, ny),
		},
		{
			name:        "season membership expires dec 31 of purchase year",
			passType:    domain.PassTypeSeason,
			purchasedAt: time.Date(2026, 4, 1, 8, 0, 0, 0, ny),
			want:        time.Date(2026, 12, 31, 23, 59, 59, 0, ny),
		},
		{
			name:        "season membership bought on dec 31 expires same day",
			passType:    domain.PassTypeSeason,
			purchasedAt: time.Date(2026, 12, 31, 22, 0, 0, 0, ny),
			want:        time.Date(2026, 12, 31, 23, 59, 59, 0, ny),
		},
		{
			name:        "season membership year uses club timezone not utc",
			passType:    domain.PassTypeSeason,
			purchasedAt: time.Date(2027, 1, 1, 3, 0, 0, 0, time.UTC), // 2026-12-31 22:00 ET
			want:        time.Date(2026, 12, 31, 23, 59, 59, 0, ny),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expiry.Calculate(tc.passType, tc.purchasedAt, ny)
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("Calculate = %s, want %s", got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			if gotZone, _ := got.Zone(); gotZone == "UTC" {
				t.Errorf("expected club-local zone, got UTC for %s", got)
			}
		})
	}
}

func TestCalculateRejectsUnknownPassType(t *testing.T) {
	t.Parallel()
	ny := mustLoad(t, "America/New_York")
	if _, err := expiry.Calculate(domain.PassType("mystery"), time.Now(), ny); !errors.Is(err, domain.ErrUnknownPassType) {
		t.Fatalf("err = %v, want ErrUnknownPassType", err)
	}
}

func TestCalculateRequiresPurchaseDate(t *testing.T) {
	t.Parallel()
	ny := mustLoad(t, "America/New_York")
	if _, err := expiry.Calculate(domain.PassTypeDay, time.Time{}, ny); err == nil {
		t.Fatal("expected error for zero purchase time")
	}
}

func TestCalculateDefaultsToUTCWhenLocationNil(t *testing.T) {
	t.Parallel()
	got, err := expiry.Calculate(domain.PassTypeDay, time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	want := time.Date(2026, 7, 5, 23, 59, 59, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Calculate = %s, want %s", got, want)
	}
}
