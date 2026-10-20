package passes

// Pass rendering: the gallery's "Classic status band" design at phone size
// (1080 x 1920), as PNG, and an A6 PDF for printing. Pure Go, so the same
// code renders in tests, the API and the worker with no external tools
// (docs/decisions/0007-pass-rendering-in-go.md).

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed fonts/*.ttf
var fontFS embed.FS

// Card is everything printed on a pass.
type Card struct {
	OrgName   string
	EventName string
	When      string // e.g. "3–4 Oct 2026"
	Where     string
	FullName  string
	PassID    string
	Status    string // registered | pending | waitlisted | cancelled
	Token     string // QR content
	Lines     []Line // answers marked "printed on the pass"
}

// Line is one labelled answer.
type Line struct{ Label, Value string }

const (
	cardW = 1080
	cardH = 1920
	pad   = 72
)

// Status band colors and labels (design tokens; status colors never change
// per tenant). Dark enough for white text at 4.5:1.
var bands = map[string]struct {
	bg    color.RGBA
	label string
}{
	"registered": {rgb(0x2350C8), "REGISTERED"},
	"pending":    {rgb(0x8A5509), "AWAITING APPROVAL"},
	"waitlisted": {rgb(0x4A5560), "WAITLIST"},
	"cancelled":  {rgb(0xB83A32), "CANCELLED"},
}

var (
	ink     = rgb(0x15202B)
	muted   = rgb(0x5B6874)
	surface = rgb(0xFFFFFF)
	line    = rgb(0xD3DBD8)
)

func rgb(v uint32) color.RGBA {
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xFF}
}

type canvas struct {
	img *image.RGBA
	err error
}

func (c *canvas) rect(x0, y0, x1, y1 int, col color.Color) {
	draw.Draw(c.img, image.Rect(x0, y0, x1, y1), &image.Uniform{col}, image.Point{}, draw.Src)
}

// qr draws the token as a QR code of whole-pixel modules, centered in a
// size x size white square at (x, y). The quiet zone (4 modules, spec's scan
// check) comes from the encoder's border.
func (c *canvas) qr(token string, x, y, size int) {
	q, err := qrcode.New(token, qrcode.Medium)
	if err != nil {
		c.err = err
		return
	}
	bm := q.Bitmap() // includes a 4-module border
	n := len(bm)
	module := size / n
	off := (size - module*n) / 2
	c.rect(x, y, x+size, y+size, surface)
	for r, row := range bm {
		for col, dark := range row {
			if dark {
				px, py := x+off+col*module, y+off+r*module
				c.rect(px, py, px+module, py+module, ink)
			}
		}
	}
}

// Render draws the pass.
func Render(card Card) (*image.RGBA, error) {
	if err := loadFonts(); err != nil {
		return nil, err
	}
	renderMu.Lock()
	defer renderMu.Unlock()

	c := &canvas{img: image.NewRGBA(image.Rect(0, 0, cardW, cardH))}
	c.rect(0, 0, cardW, cardH, surface)
	band, ok := bands[card.Status]
	if !ok {
		band = bands["registered"]
	}
	w := cardW - 2*pad
	text := func(s string, fs fontSet, max, min float64, lines int, col color.Color, y int, lh float64) int {
		l, size := fit(s, fs, max, min, w, lines)
		return drawLines(c.img, l, size, col, pad, y, w, lh)
	}

	// Status band, readable at arm's length.
	c.rect(0, 0, cardW, 190, band.bg)
	text(band.label, bold, 64, 40, 1, surface, 62, 1)

	y := text(strings.ToUpper(card.OrgName), bold, 34, 26, 1, muted, 250, 1.3)
	y = text(card.EventName, bold, 56, 38, 2, ink, y+4, 1.2)
	y = text(card.FullName, bold, 96, 52, 2, ink, y+40, 1.15)
	for _, l := range card.Lines {
		y = text(l.Label+": "+l.Value, regular, 38, 30, 1, muted, y+18, 1.3)
	}

	// QR: 640 px is about 59% of the phone width (spec: at least 40%).
	qrSize := 640
	qrY := max(y+40, 900)
	if qrY+qrSize > cardH-300 {
		qrY = cardH - 300 - qrSize
	}
	c.qr(card.Token, (cardW-qrSize)/2, qrY, qrSize)

	y = text(card.PassID, bold, 60, 40, 1, ink, qrY+qrSize+28, 1.25)
	c.rect(pad, y+16, cardW-pad, y+18, line)
	when := card.When
	if card.Where != "" {
		when += " · " + card.Where
	}
	text(when, regular, 40, 30, 2, muted, y+44, 1.3)

	if c.err != nil {
		return nil, c.err
	}
	return c.img, nil
}

// PNG renders the pass as PNG bytes.
func PNG(card Card) ([]byte, error) {
	img, err := Render(card)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PDF renders the pass on an A6 page (105 x 148 mm), centered, for printing
// at actual size. One JPEG image, so no PDF library is needed.
func PDF(card Card) ([]byte, error) {
	img, err := Render(card)
	if err != nil {
		return nil, err
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, img, &jpeg.Options{Quality: 92}); err != nil {
		return nil, err
	}
	const mm = 72 / 25.4
	pageW, pageH := 105*mm, 148*mm
	imgH := pageH - 8*mm
	imgW := imgH * cardW / cardH
	x, y := (pageW-imgW)/2, 4*mm
	content := fmt.Sprintf("q %.3f 0 0 %.3f %.3f %.3f cm /Im0 Do Q\n", imgW, imgH, x, y)

	var out bytes.Buffer
	offsets := make([]int, 7)
	obj := func(n int, body string) {
		offsets[n] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", n, body)
	}
	out.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")
	obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	obj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj(3, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.3f %.3f] /Resources << /XObject << /Im0 4 0 R >> >> /Contents 5 0 R >>", pageW, pageH))
	offsets[4] = out.Len()
	fmt.Fprintf(&out, "4 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n",
		cardW, cardH, jpg.Len())
	out.Write(jpg.Bytes())
	out.WriteString("\nendstream\nendobj\n")
	obj(5, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
	obj(6, "<< /Title ("+pdfString(card.EventName+" pass")+") /Producer (nsac-qr) >>")
	xref := out.Len()
	out.WriteString("xref\n0 7\n0000000000 65535 f \n")
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size 7 /Root 1 0 R /Info 6 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return out.Bytes(), nil
}

// pdfString escapes a PDF literal string, keeping printable ASCII only.
func pdfString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '(' || r == ')' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 32 && r < 127:
			b.WriteRune(r)
		}
	}
	return b.String()
}
