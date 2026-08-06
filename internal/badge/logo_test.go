package badge

import (
	"bytes"
	"image/color"
	"image/png"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/domain"
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

func TestLogoWordmarkScalesUpForRetinaStrip(t *testing.T) {
	t.Parallel()
	// logo@2x.png (320x100) has more than double the available width of
	// logo.png (160x50) once the panel is subtracted. The chosen scale must
	// reflect that: a fit loop that only ever steps scale *down* would settle
	// on the same scale for both, and the @2x asset would render at half its
	// intended size on a Retina display.
	_, scale1x := fitWordmark(160, 50)
	_, scale2x := fitWordmark(320, 100)
	if scale2x <= scale1x {
		t.Errorf("2x scale = %d, want strictly greater than 1x scale = %d", scale2x, scale1x)
	}
}

func TestFitWordmarkPicksLargestScaleSatisfyingBothConstraints(t *testing.T) {
	t.Parallel()
	// A wide-but-short strip: width leaves ample room, so the height
	// constraint (13*s <= height) is the binding one. A starting bound
	// derived from an arbitrary fraction of height (e.g. height/16) can
	// understate the true height-derived cap (height/13) and thereby skip a
	// larger scale that legitimately satisfies both constraints.
	const width, height = 2000, 100
	_, scale := fitWordmark(width, height)
	if want := 7; scale != want {
		t.Errorf("fitWordmark(%d, %d) scale = %d, want %d (largest scale with 13*scale <= height)", width, height, scale, want)
	}
}

func TestRenderPlacesLogoTopRight(t *testing.T) {
	t.Parallel()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	b := domain.Badge{
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
	data, err := Render(b, ny)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Panel card in the top-right region.
	if !isWhitish(img.At(840, 70)) {
		t.Errorf("pixel at (840,70) = %v, want the whitish card", img.At(840, 70))
	}
	// Background is untouched elsewhere.
	if got := img.At(500, 500); !sameRGB(got, colorBackground) {
		t.Errorf("pixel at (500,500) = %v, want background %v", got, colorBackground)
	}
	// The hairline stops at x=700, and the panel starts at x=740, so the gap
	// between them must be plain background. Sampling inside the panel would be
	// useless: the white card covers the hairline either way.
	if got := img.At(720, 152); !sameRGB(got, colorBackground) {
		t.Errorf("pixel at (720,152) = %v, want background %v (hairline not shortened)", got, colorBackground)
	}
}

func sameRGB(got, want color.Color) bool {
	gr, gg, gb, _ := got.RGBA()
	wr, wg, wb, _ := want.RGBA()
	return gr == wr && gg == wg && gb == wb
}
