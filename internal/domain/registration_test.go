package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/domain"
)

func TestClassifyPassType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw     string
		want    domain.PassType
		wantErr bool
	}{
		{raw: "Day Pass", want: domain.PassTypeDay},
		{raw: "  day   pass  ", want: domain.PassTypeDay},
		{raw: "DAYPASS", want: domain.PassTypeDay},
		{raw: "North Landing DGC - Day Pass ($5)", want: domain.PassTypeDay},
		{raw: "Season Membership", want: domain.PassTypeSeason},
		{raw: "2026 Season Membership - $50", want: domain.PassTypeSeason},
		{raw: "annual membership", want: domain.PassTypeSeason},
		{raw: "", wantErr: true},
		{raw: "   ", wantErr: true},
		{raw: "Tournament Entry", wantErr: true},
		{raw: "Weekly League", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := domain.ClassifyPassType(tc.raw)
			if tc.wantErr {
				if !errors.Is(err, domain.ErrUnknownPassType) {
					t.Fatalf("ClassifyPassType(%q) err = %v, want ErrUnknownPassType", tc.raw, err)
				}
				if got != "" {
					t.Fatalf("ClassifyPassType(%q) = %q, want empty on error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ClassifyPassType(%q) unexpected err: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ClassifyPassType(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestPassTypeLabel(t *testing.T) {
	t.Parallel()
	if got := domain.PassTypeDay.Label(); got != "Day Pass" {
		t.Errorf("day label = %q", got)
	}
	if got := domain.PassTypeSeason.Label(); got != "Season Member" {
		t.Errorf("season label = %q", got)
	}
}

func TestRegistrationValidate(t *testing.T) {
	t.Parallel()
	valid := domain.Registration{
		ID:          "ORD-1",
		Name:        "Casey Chains",
		Email:       "casey@example.com",
		RawPassType: "Day Pass",
		PurchasedAt: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid registration rejected: %v", err)
	}

	tests := map[string]func(r *domain.Registration){
		"no id":    func(r *domain.Registration) { r.ID = " " },
		"no name":  func(r *domain.Registration) { r.Name = "" },
		"no email": func(r *domain.Registration) { r.Email = "" },
		"no date":  func(r *domain.Registration) { r.PurchasedAt = time.Time{} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r := valid
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}
