package dgs_test

import (
	"errors"
	"testing"

	"github.com/northlanding/badges/internal/dgs"
	"github.com/northlanding/badges/internal/domain"
)

func TestClassifyDivision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		division string
		want     domain.PassType
	}{
		{"MEM", domain.PassTypeSeason},
		{"mem", domain.PassTypeSeason},
		{"  MEM  ", domain.PassTypeSeason},
		{"FNDR", domain.PassTypeFounder},
		{"fndr", domain.PassTypeFounder},
		{"SPON", domain.PassTypeSponsor},
		{"spon", domain.PassTypeSponsor},
	}
	for _, tc := range cases {
		got, err := dgs.ClassifyDivision(tc.division)
		if err != nil {
			t.Errorf("ClassifyDivision(%q) error = %v, want nil", tc.division, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ClassifyDivision(%q) = %q, want %q", tc.division, got, tc.want)
		}
	}
}

func TestClassifyDivisionUnknown(t *testing.T) {
	t.Parallel()
	for _, division := range []string{"", "PRO", "MA1", "Totals"} {
		if _, err := dgs.ClassifyDivision(division); !errors.Is(err, domain.ErrUnknownPassType) {
			t.Errorf("ClassifyDivision(%q) error = %v, want ErrUnknownPassType", division, err)
		}
	}
}

func TestIsTotalsRow(t *testing.T) {
	t.Parallel()
	for _, division := range []string{"Totals", "totals", " TOTALS "} {
		if !dgs.IsTotalsRow(division) {
			t.Errorf("IsTotalsRow(%q) = false, want true", division)
		}
	}
	for _, division := range []string{"MEM", "FNDR", "SPON", ""} {
		if dgs.IsTotalsRow(division) {
			t.Errorf("IsTotalsRow(%q) = true, want false", division)
		}
	}
}
