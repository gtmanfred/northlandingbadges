package badge

import (
	"bytes"
	"image/color"
	"image/png"
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

func TestIconDrawsLogoPanel(t *testing.T) {
	t.Parallel()
	data, err := Icon(58)
	if err != nil {
		t.Fatalf("Icon: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !isWhitish(img.At(29, 4)) {
		t.Errorf("top-centre pixel = %v, want the whitish card", img.At(29, 4))
	}

	found := false
	for y := 14; y < 44 && !found; y++ {
		for x := 14; x < 44; x++ {
			if isGreenish(img.At(x, y)) {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("no green-dominant pixel; the icon is not drawing the club mark")
	}
}

func TestLogoPlacesPanelLeftOfWordmark(t *testing.T) {
	t.Parallel()
	const w, h = 320, 100
	data, err := Logo(w, h)
	if err != nil {
		t.Fatalf("Logo: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The panel occupies the leading h×h square.
	if !isWhitish(img.At(h/2, 6)) {
		t.Errorf("pixel inside the panel = %v, want whitish card", img.At(h/2, 6))
	}

	// The wordmark sits to the right of the panel, drawn in the accent colour.
	accent := colorAccent
	found := false
	for x := h; x < w && !found; x++ {
		for y := 0; y < h; y++ {
			r, g, b, a := img.At(x, y).RGBA()
			ar, ag, ab, _ := accent.RGBA()
			if a > 0x8000 && r == ar && g == ag && b == ab {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("no accent-coloured wordmark pixel to the right of the panel")
	}
}

func TestLogoFitsWordmarkInNarrowStrip(t *testing.T) {
	t.Parallel()
	// 160x50 is the pkpass logo.png size. The wordmark must not overflow the
	// canvas once the panel takes its 50px.
	data, err := Logo(160, 50)
	if err != nil {
		t.Fatalf("Logo: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if img.Bounds().Dx() != 160 || img.Bounds().Dy() != 50 {
		t.Errorf("bounds = %v, want 160x50", img.Bounds())
	}
	if !isWhitish(img.At(25, 4)) {
		t.Errorf("pixel inside the panel = %v, want whitish card", img.At(25, 4))
	}
}
