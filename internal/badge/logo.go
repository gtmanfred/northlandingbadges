package badge

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync"

	"golang.org/x/image/draw"
)

//go:embed assets/logo.png
var logoFS embed.FS

// LogoAssetPath is the repo-relative path of the embedded club mark. The
// googlepass package builds Google's public image URL from it, so the two
// cannot drift apart when the file moves.
const LogoAssetPath = "internal/badge/assets/logo.png"

// logoContentDescription is the alt text used wherever the mark is published
// with an accessibility field.
const logoContentDescription = "North Landing Disc Golf Club logo"

var (
	logoOnce sync.Once
	logoImg  image.Image
	logoErr  error
)

// logoMark returns the decoded club mark. The bytes are compiled in, so a
// failure here is a build-time problem rather than a runtime one, but it is
// returned as an error rather than panicking: the service must stay up.
//
// Decoding is cached because Render runs once per registration and a cycle
// processes hundreds of them on a 256MB instance.
func logoMark() (image.Image, error) {
	logoOnce.Do(func() {
		data, err := logoFS.ReadFile("assets/logo.png")
		if err != nil {
			logoErr = fmt.Errorf("badge: read embedded logo: %w", err)
			return
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			logoErr = fmt.Errorf("badge: decode embedded logo: %w", err)
			return
		}
		logoImg = img
	})
	return logoImg, logoErr
}

// logoPanel draws the club mark centred on a white rounded card of size×size.
//
// The mark is dark green on white, and every surface it lands on is also dark
// green, so a transparent knockout would be invisible. The card keeps the
// artwork legible on any background. Corners outside the radius are left fully
// transparent so the surface behind shows through.
func logoPanel(size int) (*image.RGBA, error) {
	if size < 8 {
		return nil, fmt.Errorf("badge: logo panel size %d too small", size)
	}
	mark, err := logoMark()
	if err != nil {
		return nil, err
	}

	panel := image.NewRGBA(image.Rect(0, 0, size, size))
	radius := size / 8
	drawRoundedRect(panel, size, radius, color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF})

	// Inset the artwork ~8% so the card reads as a border rather than a crop.
	inset := size / 12
	target := image.Rect(inset, inset, size-inset, size-inset)
	// CatmullRom, not NearestNeighbor: at a 29px icon the NL/DG lettering
	// disintegrates under nearest-neighbour downscaling.
	draw.CatmullRom.Scale(panel, target, mark, mark.Bounds(), draw.Over, nil)

	return panel, nil
}

// drawRoundedRect fills a size×size rounded square with c, leaving the pixels
// outside the corner radius untouched (transparent).
func drawRoundedRect(dst *image.RGBA, size, radius int, c color.RGBA) {
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if insideRounded(x, y, size, radius) {
				dst.SetRGBA(x, y, c)
			}
		}
	}
}

// insideRounded reports whether (x,y) falls inside a rounded square.
func insideRounded(x, y, size, radius int) bool {
	if radius <= 0 {
		return true
	}
	// Distance from the nearest corner centre, only checked in the corners.
	cx, cy := x, y
	switch {
	case x < radius && y < radius:
		cx, cy = radius, radius
	case x >= size-radius && y < radius:
		cx, cy = size-radius-1, radius
	case x < radius && y >= size-radius:
		cx, cy = radius, size-radius-1
	case x >= size-radius && y >= size-radius:
		cx, cy = size-radius-1, size-radius-1
	default:
		return true
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= radius*radius
}
