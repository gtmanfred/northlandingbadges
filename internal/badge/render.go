// Package badge renders the visual badge artwork: the PNG shown in the email
// and the icon/logo images an Apple .pkpass bundle requires.
//
// Text is drawn with the stdlib-adjacent basicfont bitmap face and scaled with
// nearest-neighbour so the package needs no font file on disk — important for a
// scratch container image.
package badge

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/northlanding/badges/internal/domain"
)

// Palette is North Landing's badge colour scheme.
var (
	colorBackground = color.RGBA{R: 0x0B, G: 0x3D, B: 0x2E, A: 0xFF}
	colorAccent     = color.RGBA{R: 0xE8, G: 0xB4, B: 0x3A, A: 0xFF}
	colorText       = color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	colorMuted      = color.RGBA{R: 0xB9, G: 0xD4, B: 0xC8, A: 0xFF}
)

// Tier accent colours. The gold above is North Landing's default; founders and
// sponsors get their own so the tiers are distinguishable at a glance.
var (
	colorAccentFounder = color.RGBA{R: 0xC9, G: 0xD3, B: 0xDB, A: 0xFF}
	colorAccentSponsor = color.RGBA{R: 0xB8, G: 0x73, B: 0x33, A: 0xFF}
)

// AccentColor is the accent used for a pass type's artwork.
func AccentColor(p domain.PassType) color.RGBA {
	switch p {
	case domain.PassTypeFounder:
		return colorAccentFounder
	case domain.PassTypeSponsor:
		return colorAccentSponsor
	default:
		return colorAccent
	}
}

// Width and Height are the badge PNG dimensions.
const (
	Width  = 1000
	Height = 560
)

// DateLayout is how expirations are printed on artwork and in email copy.
const DateLayout = "Mon, Jan 2 2006 at 3:04 PM MST"

// Render draws the badge for b and returns encoded PNG bytes. Times are printed
// in loc so the artwork matches the club's local expiration.
func Render(b domain.Badge, loc *time.Location) ([]byte, error) {
	if loc == nil {
		loc = time.UTC
	}
	if err := b.Registration.Validate(); err != nil {
		return nil, fmt.Errorf("badge: %w", err)
	}

	accent := AccentColor(b.PassType)

	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	fill(img, img.Bounds(), colorBackground)
	// Accent bar down the left edge and a hairline under the header.
	fill(img, image.Rect(0, 0, 18, Height), accent)
	fill(img, image.Rect(60, 150, Width-60, 154), accent)

	// Founder badges have no expiration, so the zero ExpiresAt is never formatted.
	expiresLine := "NO EXPIRATION"
	if b.Expires() {
		expiresLine = "EXPIRES " + strings.ToUpper(b.ExpiresAt.In(loc).Format(DateLayout))
	}

	drawScaled(img, 60, 60, 3, accent, "NORTH LANDING DGC")
	drawScaled(img, 60, 200, 5, colorText, strings.ToUpper(truncate(b.Registration.Name, 22)))
	drawScaled(img, 60, 300, 3, accent, strings.ToUpper(b.PassType.Label()))
	drawScaled(img, 60, 380, 2, colorMuted, expiresLine)
	drawScaled(img, 60, 440, 2, colorMuted, "MEMBER "+truncate(b.Registration.ID, 32))
	drawScaled(img, 60, 490, 1, colorMuted, "Present this badge at North Landing DGC. Not transferable.")

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("badge: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// Icon renders the square club icon at the requested pixel size. Apple requires
// icon.png at 29pt plus @2x/@3x variants.
func Icon(size int) ([]byte, error) {
	if size < 8 {
		return nil, fmt.Errorf("badge: icon size %d too small", size)
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	fill(img, img.Bounds(), colorBackground)

	panel, err := logoPanel(size)
	if err != nil {
		return nil, err
	}
	draw.Draw(img, img.Bounds(), panel, image.Point{}, draw.Over)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("badge: encode icon: %w", err)
	}
	return buf.Bytes(), nil
}

// Logo renders the wide wordmark used as the .pkpass logo.
func Logo(width, height int) ([]byte, error) {
	if width < 32 || height < 12 {
		return nil, fmt.Errorf("badge: logo %dx%d too small", width, height)
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fill(img, img.Bounds(), colorBackground)
	scale := max(1, height/16)
	drawScaled(img, 4, (height-13*scale)/2, scale, colorAccent, "NORTH LANDING DGC")

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("badge: encode logo: %w", err)
	}
	return buf.Bytes(), nil
}

func fill(img *image.RGBA, r image.Rectangle, c color.Color) {
	draw.Draw(img, r, image.NewUniform(c), image.Point{}, draw.Src)
}

// drawScaled renders s with the 7x13 bitmap face, magnified by scale, with its
// top-left corner at (x, y).
func drawScaled(dst *image.RGBA, x, y, scale int, c color.Color, s string) {
	if scale < 1 {
		scale = 1
	}
	face := basicfont.Face7x13
	w := face.Advance * len([]rune(s))
	h := face.Metrics().Height.Ceil() + face.Descent
	if w <= 0 || h <= 0 {
		return
	}

	small := image.NewRGBA(image.Rect(0, 0, w, h))
	d := &font.Drawer{
		Dst:  small,
		Src:  image.NewUniform(c),
		Face: face,
		Dot:  fixed.P(0, face.Ascent),
	}
	d.DrawString(s)

	if scale == 1 {
		draw.Draw(dst, image.Rect(x, y, x+w, y+h), small, image.Point{}, draw.Over)
		return
	}
	target := image.Rect(x, y, x+w*scale, y+h*scale)
	draw.NearestNeighbor.Scale(dst, target, small, small.Bounds(), draw.Over, nil)
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
