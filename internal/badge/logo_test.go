package badge

import (
	"image/color"
	"testing"
)

func TestLogoAssetDecodes(t *testing.T) {
	t.Parallel()
	img, err := logoMark()
	if err != nil {
		t.Fatalf("logoMark: %v", err)
	}
	if got := img.Bounds().Dx(); got != 504 {
		t.Errorf("width = %d, want 504", got)
	}
	if got := img.Bounds().Dy(); got != 504 {
		t.Errorf("height = %d, want 504", got)
	}
}

func TestLogoAssetPathPointsAtTheEmbeddedFile(t *testing.T) {
	t.Parallel()
	if LogoAssetPath != "internal/badge/assets/logo.png" {
		t.Errorf("LogoAssetPath = %q", LogoAssetPath)
	}
}

// isWhitish reports whether c is close enough to white to be the panel card.
// Exact equality is wrong here: CatmullRom resampling blends edge pixels.
func isWhitish(c color.Color) bool {
	r, g, b, a := c.RGBA()
	return a > 0x8000 && r > 0xE000 && g > 0xE000 && b > 0xE000
}

// isGreenish reports whether c is green-dominant, i.e. part of the club mark.
func isGreenish(c color.Color) bool {
	r, g, b, a := c.RGBA()
	return a > 0x8000 && g > r && g > b
}

func TestLogoPanelDrawsWhiteCardWithGreenMark(t *testing.T) {
	t.Parallel()
	const size = 200
	panel, err := logoPanel(size)
	if err != nil {
		t.Fatalf("logoPanel: %v", err)
	}
	if panel.Bounds().Dx() != size || panel.Bounds().Dy() != size {
		t.Fatalf("bounds = %v, want %dx%d", panel.Bounds(), size, size)
	}

	// The card fills the middle of the panel.
	if !isWhitish(panel.At(size/2, 8)) {
		t.Errorf("top-centre pixel = %v, want whitish card", panel.At(size/2, 8))
	}

	// Rounded corners leave the extreme corner transparent so the badge
	// background shows through.
	if _, _, _, a := panel.At(0, 0).RGBA(); a != 0 {
		t.Errorf("corner alpha = %d, want 0 (rounded corner)", a)
	}

	// Somewhere in the middle there must be actual green artwork.
	found := false
	for y := size / 4; y < 3*size/4 && !found; y++ {
		for x := size / 4; x < 3*size/4; x++ {
			if isGreenish(panel.At(x, y)) {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("no green-dominant pixel found; the club mark was not composited")
	}
}

func TestLogoPanelRejectsTinySize(t *testing.T) {
	t.Parallel()
	if _, err := logoPanel(4); err == nil {
		t.Error("expected an error for a tiny panel")
	}
}
