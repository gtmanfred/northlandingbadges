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
		{raw: "North Landing Community - Day Pass ($5)", want: domain.PassTypeDay},
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

func TestPassTypeLabelIncludesTiers(t *testing.T) {
	t.Parallel()
	cases := map[domain.PassType]string{
		domain.PassTypeDay:     "Day Pass",
		domain.PassTypeSeason:  "Season Member",
		domain.PassTypeFounder: "Founder",
		domain.PassTypeSponsor: "Course Sponsor",
	}
	for passType, want := range cases {
		if got := passType.Label(); got != want {
			t.Errorf("Label(%q) = %q, want %q", passType, got, want)
		}
	}
}

func TestBadgeExpires(t *testing.T) {
	t.Parallel()
	expiring := domain.Badge{ExpiresAt: time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)}
	if !expiring.Expires() {
		t.Error("badge with a non-zero ExpiresAt should report Expires() == true")
	}
	permanent := domain.Badge{}
	if permanent.Expires() {
		t.Error("badge with a zero ExpiresAt should report Expires() == false")
	}
}

func TestCandidateCarriesRegistrationAndPassType(t *testing.T) {
	t.Parallel()
	c := domain.Candidate{
		Registration: domain.Registration{ID: "abc123", Name: "A Member", Email: "a@example.com"},
		PassType:     domain.PassTypeFounder,
	}
	if c.Registration.ID != "abc123" || c.PassType != domain.PassTypeFounder {
		t.Errorf("candidate = %+v", c)
	}
}

func TestRegistrationSeasonYearIsCarried(t *testing.T) {
	t.Parallel()
	reg := domain.Registration{
		ID:          "1",
		Name:        "A Member",
		Email:       "a@example.com",
		SeasonYear:  2026,
		PurchasedAt: time.Date(2025, 11, 13, 1, 7, 27, 0, time.UTC),
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if reg.SeasonYear != 2026 {
		t.Errorf("SeasonYear = %d, want 2026", reg.SeasonYear)
	}
}

func TestPDGAHelpers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		number  string
		hasPDGA bool
		url     string
	}{
		{"a number on file", "12345", true, "https://www.pdga.com/player/12345"},
		{"no number", "", false, ""},
		{"whitespace only", "   ", false, ""},
		{"surrounding whitespace is trimmed", " 987 ", true, "https://www.pdga.com/player/987"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := domain.Registration{PDGANumber: tc.number}
			if got := r.HasPDGA(); got != tc.hasPDGA {
				t.Errorf("HasPDGA() = %v, want %v", got, tc.hasPDGA)
			}
			if got := r.PDGAURL(); got != tc.url {
				t.Errorf("PDGAURL() = %q, want %q", got, tc.url)
			}
		})
	}
}

func TestPDGALabel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		number string
		want   string
	}{
		{"a number on file", "12345", "PDGA #12345"},
		{"no number", "", ""},
		{"whitespace-padded", " 12345 ", "PDGA #12345"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := domain.Registration{PDGANumber: tc.number}
			if got := r.PDGALabel(); got != tc.want {
				t.Errorf("PDGALabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateIgnoresPDGANumber(t *testing.T) {
	t.Parallel()
	// A PDGA number is optional: its absence must never cost a registrant a badge.
	ny, _ := time.LoadLocation("America/New_York")
	r := domain.Registration{
		ID:          "abc123def456",
		Name:        "Casey Chains",
		Email:       "casey@example.com",
		PurchasedAt: time.Date(2026, 7, 4, 10, 0, 0, 0, ny),
	}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate() with no PDGA number = %v, want nil", err)
	}
}
