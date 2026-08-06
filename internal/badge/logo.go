package badge

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/image/draw"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

//go:embed assets/logo.png
var logoFS embed.FS

// LogoAssetPath is the repo-relative path of the embedded club mark. The
// googlepass package builds Google's public image URL from it, so the two
// cannot drift apart when the file moves.
const LogoAssetPath = "internal/badge/assets/logo.png"

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

// qrPanelInset is how far the code is inset inside its white card. Deliberately
// tighter than the logo panel's size/12: padding costs QR modules, and at the
// logo's inset a 200px card leaves only 168px of code at roughly 4.5px per
// module. Scannability outranks matching the logo's padding exactly.
const qrPanelInset = 20

// qrPanel renders url as a QR code on a rounded white card of size×size pixels.
//
// The card reuses the logo panel's rounded-corner mask so the two right-column
// elements on the badge read as one design language; a hard-edged square below a
// rounded card looks unintentional.
//
// Medium error correction is the sensible default for a code viewed in an email:
// it survives some scaling without inflating the module count the way High would.
func qrPanel(url string, size int) (*image.RGBA, error) {
	if size < 32 {
		return nil, fmt.Errorf("badge: qr size %d too small", size)
	}
	code, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("badge: encode qr: %w", err)
	}

	panel := image.NewRGBA(image.Rect(0, 0, size, size))
	drawRoundedRect(panel, size, size/8, color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF})

	inset := size / qrPanelInset
	src := code.Image(size - 2*inset)
	draw.Draw(panel, image.Rect(inset, inset, size-inset, size-inset), src, src.Bounds().Min, draw.Src)
	return panel, nil
}

// asciiFold maps Latin-1 and Latin Extended-A letters onto the ASCII letters
// basicfont.Face7x13 can actually draw. A registrant named "José" must read as
// "JOSE" rather than "JOS" followed by a replacement box.
var asciiFold = map[rune]string{
	'À': "A", 'Á': "A", 'Â': "A", 'Ã': "A", 'Ä': "A", 'Å': "A", 'Æ': "AE",
	'Ç': "C", 'È': "E", 'É': "E", 'Ê': "E", 'Ë': "E",
	'Ì': "I", 'Í': "I", 'Î': "I", 'Ï': "I",
	'Ñ': "N", 'Ò': "O", 'Ó': "O", 'Ô': "O", 'Õ': "O", 'Ö': "O", 'Ø': "O",
	'Ù': "U", 'Ú': "U", 'Û': "U", 'Ü': "U", 'Ý': "Y",
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'æ': "ae",
	'ç': "c", 'è': "e", 'é': "e", 'ê': "e", 'ë': "e",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i",
	'ñ': "n", 'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ý': "y", 'ÿ': "y",
	'ß': "ss", 'Œ': "OE", 'œ': "oe",
}

// drawable returns s with accented Latin letters folded to ASCII and any
// remaining rune basicfont cannot draw removed. Dropping beats substituting: a
// name rendered "JOS?" reads as corrupt data, while a filled box reads as a bug.
func drawable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if folded, ok := asciiFold[r]; ok {
			b.WriteString(folded)
			continue
		}
		if _, _, _, _, ok := basicfont.Face7x13.Glyph(fixed.P(0, 0), r); ok {
			b.WriteRune(r)
		}
	}
	return b.String()
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
