package badge

import (
	"bytes"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

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

func TestNameCannotRunUnderLogoPanel(t *testing.T) {
	t.Parallel()
	// drawScaled advances 7px per glyph per scale unit, starting at x=60.
	rightEdge := 60 + basicfont.Face7x13.Advance*maxNameChars*nameScale
	if rightEdge >= logoPanelX {
		t.Errorf("a max-length name reaches x=%d, which is inside the panel starting at x=%d",
			rightEdge, logoPanelX)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"shorter than n returned unchanged", "Casey", 22, "Casey"},
		{"exact length returned unchanged", "Casey", 5, "Casey"},
		{"longer than n elides with three periods", "Bartholomew Fitzwilliam-Chainsworth", 19, "Bartholomew Fitz..."},
		{"n at the floor uses a hard cut, no periods", "Bartholomew", 3, "Bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.s, tt.n)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
			}
			if r := []rune(got); len(r) > tt.n {
				t.Errorf("truncate(%q, %d) = %q, has %d runes, want at most %d", tt.s, tt.n, got, len(r), tt.n)
			}
			for _, r := range got {
				if r == '…' {
					t.Errorf("truncate(%q, %d) = %q, contains U+2026 (ellipsis rune)", tt.s, tt.n, got)
				}
			}
		})
	}
}

func TestTruncateOutputIsRenderableByBasicFont(t *testing.T) {
	t.Parallel()
	// A long input forces elision. Every rune of the result must have a glyph
	// in basicfont.Face7x13, the face render.go draws with; otherwise it
	// renders as a replacement box on the badge artwork.
	got := truncate("Bartholomew Fitzwilliam-Chainsworth", maxNameChars)
	for _, r := range got {
		if _, _, _, _, ok := basicfont.Face7x13.Glyph(fixed.P(0, 0), r); !ok {
			t.Errorf("truncate output %q contains rune %q with no glyph in basicfont.Face7x13", got, r)
		}
	}
}

func TestDrawableFoldsAccentsToASCII(t *testing.T) {
	t.Parallel()
	got := drawable("José Müller")
	want := "Jose Muller"
	if got != want {
		t.Errorf("drawable(%q) = %q, want %q", "José Müller", got, want)
	}
}

func TestDrawableDropsUnrenderableRunes(t *testing.T) {
	t.Parallel()
	// basicfont.Face7x13 has no CJK glyphs and asciiFold has no entries for
	// them, so they are dropped outright rather than substituted.
	got := drawable("中文 name")
	want := " name"
	if got != want {
		t.Errorf("drawable(%q) = %q, want %q", "中文 name", got, want)
	}
}

func TestDrawableOutputIsRenderableByBasicFont(t *testing.T) {
	t.Parallel()
	got := drawable("José Müller, 中文 Naïve façade Straße")
	for _, r := range got {
		if _, _, _, _, ok := basicfont.Face7x13.Glyph(fixed.P(0, 0), r); !ok {
			t.Errorf("drawable output %q contains rune %q with no glyph in basicfont.Face7x13", got, r)
		}
	}
}

func TestRenderAppliesDrawableToName(t *testing.T) {
	t.Parallel()
	b := domain.Badge{
		Registration: domain.Registration{
			ID:          "DGS-88231",
			Name:        "José Müller",
			Email:       "jose@example.com",
			RawPassType: "Day Pass",
			PurchasedAt: time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC),
		},
		PassType:  domain.PassTypeDay,
		ExpiresAt: time.Date(2026, 7, 5, 23, 59, 59, 0, time.UTC),
	}
	if _, err := Render(b, time.UTC); err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := truncate(drawable(b.Registration.Name), maxNameChars)
	want := "Jose Muller"
	if got != want {
		t.Errorf("truncate(drawable(name), maxNameChars) = %q, want %q", got, want)
	}
}

func TestDrawableThenTruncateOrderKeepsNameOffPanel(t *testing.T) {
	t.Parallel()
	// A name built from runes that expand when folded ('ß' -> "ss", 'Æ' ->
	// "AE"). If truncate ran before drawable, the pre-fold rune count would
	// pass the maxNameChars check but the folded result would exceed it and
	// run under the logo panel, re-breaking the bug fixed in commit 4346ebd.
	name := strings.Repeat("ß", maxNameChars)
	got := truncate(drawable(name), maxNameChars)
	if r := []rune(got); len(r) > maxNameChars {
		t.Errorf("truncate(drawable(%q), %d) = %q, has %d runes, want at most %d",
			name, maxNameChars, got, len(r), maxNameChars)
	}
}
