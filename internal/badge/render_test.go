package badge_test

import (
	"bytes"
	"image/color"
	"image/png"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/badge"
	"github.com/northlanding/badges/internal/domain"
)

func sampleBadge() domain.Badge {
	ny, _ := time.LoadLocation("America/New_York")
	return domain.Badge{
		Registration: domain.Registration{
			ID:          "DGS-88231",
			Name:        "Casey Chains",
			Email:       "casey@example.com",
			RawPassType: "Day Pass",
			PurchasedAt: time.Date(2026, 7, 4, 10, 0, 0, 0, ny),
		},
		PassType:  domain.PassTypeDay,
		ExpiresAt: time.Date(2026, 7, 5, 23, 59, 59, 0, ny),
	}
}

func TestRenderProducesDecodablePNG(t *testing.T) {
	t.Parallel()
	ny, _ := time.LoadLocation("America/New_York")
	data, err := badge.Render(sampleBadge(), ny)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png.Decode: %v", err)
	}
	if got := img.Bounds().Dx(); got != badge.Width {
		t.Errorf("width = %d, want %d", got, badge.Width)
	}
	if got := img.Bounds().Dy(); got != badge.Height {
		t.Errorf("height = %d, want %d", got, badge.Height)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()
	b := sampleBadge()
	first, err := badge.Render(b, time.UTC)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	second, err := badge.Render(b, time.UTC)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("badge rendering must be deterministic for golden-file testing")
	}
}

func TestRenderVariesWithContent(t *testing.T) {
	t.Parallel()
	a, err := badge.Render(sampleBadge(), time.UTC)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	other := sampleBadge()
	other.PassType = domain.PassTypeSeason
	other.Registration.Name = "Robin Rollaway"
	b, err := badge.Render(other, time.UTC)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("different badges must render differently")
	}
}

func TestRenderRejectsIncompleteRegistration(t *testing.T) {
	t.Parallel()
	b := sampleBadge()
	b.Registration.Email = ""
	if _, err := badge.Render(b, time.UTC); err == nil {
		t.Fatal("expected error for invalid registration")
	}
}

func TestRenderHandlesLongNames(t *testing.T) {
	t.Parallel()
	b := sampleBadge()
	b.Registration.Name = "Bartholomew Featherstonehaugh-Winchester III"
	if _, err := badge.Render(b, time.UTC); err != nil {
		t.Fatalf("Render with long name: %v", err)
	}
}

func TestIconAndLogo(t *testing.T) {
	t.Parallel()
	for _, size := range []int{29, 58, 87} {
		data, err := badge.Icon(size)
		if err != nil {
			t.Fatalf("Icon(%d): %v", size, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Icon(%d) decode: %v", size, err)
		}
		if img.Bounds().Dx() != size || img.Bounds().Dy() != size {
			t.Errorf("Icon(%d) bounds = %v", size, img.Bounds())
		}
	}
	if _, err := badge.Icon(4); err == nil {
		t.Error("expected error for tiny icon")
	}

	logo, err := badge.Logo(320, 100)
	if err != nil {
		t.Fatalf("Logo: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(logo))
	if err != nil {
		t.Fatalf("Logo decode: %v", err)
	}
	if img.Bounds().Dx() != 320 || img.Bounds().Dy() != 100 {
		t.Errorf("Logo bounds = %v", img.Bounds())
	}
	if _, err := badge.Logo(10, 10); err == nil {
		t.Error("expected error for tiny logo")
	}
}

func TestAccentColorPerTier(t *testing.T) {
	t.Parallel()
	cases := map[domain.PassType]color.RGBA{
		domain.PassTypeSeason:  {R: 0xE8, G: 0xB4, B: 0x3A, A: 0xFF},
		domain.PassTypeDay:     {R: 0xE8, G: 0xB4, B: 0x3A, A: 0xFF},
		domain.PassTypeFounder: {R: 0xC9, G: 0xD3, B: 0xDB, A: 0xFF},
		domain.PassTypeSponsor: {R: 0xB8, G: 0x73, B: 0x33, A: 0xFF},
	}
	for passType, want := range cases {
		if got := badge.AccentColor(passType); got != want {
			t.Errorf("AccentColor(%q) = %v, want %v", passType, got, want)
		}
	}
}

func TestRenderFounderBadgeUsesTierAccent(t *testing.T) {
	t.Parallel()
	b := domain.Badge{
		Registration: domain.Registration{
			ID: "reg-1", Name: "A Founder", Email: "f@example.com",
			PurchasedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		},
		PassType: domain.PassTypeFounder,
	}
	data, err := badge.Render(b, time.UTC)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	// The accent bar is the left-hand 18px column.
	got := color.RGBAModel.Convert(img.At(4, 300)).(color.RGBA)
	want := badge.AccentColor(domain.PassTypeFounder)
	if got != want {
		t.Errorf("accent bar = %v, want %v", got, want)
	}
}
