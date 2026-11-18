package passes

// Pass rendering: the gallery's "Classic status band" design at phone size
// (1080 x 1920), as PNG, and an A6 PDF for printing. Pure Go, so the same
// code renders in tests, the API and the worker with no external tools
// (docs/decisions/0007-pass-rendering-in-go.md).

import (
	"bytes"
	"compress/zlib"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
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
	// Answers by question key, for {{answers.<key>}} in designs.
	Answers map[string]string
	// Design is the event's pass template; nil means Classic.
	Design *Design
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

// CardVars turns a Card into design variables.
func CardVars(card Card) Vars {
	when := card.When
	if card.Where != "" {
		when += " · " + card.Where
	}
	vals := map[string]string{
		"org.name": card.OrgName, "event.name": card.EventName, "event.dateRange": card.When, "event.venue": card.Where,
		"event.when": when, "participant.fullName": card.FullName, "pass.id": card.PassID,
	}
	for k, v := range card.Answers {
		vals["answers."+k] = v
	}
	return Vars{Values: vals, Status: card.Status, Token: card.Token, Answers: card.Lines}
}

// Render draws the pass in its design (Classic when the card has none).
func Render(card Card) (*image.RGBA, error) {
	d := Classic()
	if card.Design != nil {
		d = *card.Design
	}
	img, _, err := RenderDesign(d, CardVars(card))
	return img, err
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

// PDF renders the pass on a page sized for its preset (see PrintSizes), for printing
// at actual size. One losslessly compressed image (Flate with PNG "Up"
// row prediction), so no PDF library is needed and artwork prints as
// sharp as it renders — JPEG would blur fine text and colored edges.
func PDF(card Card) ([]byte, error) {
	img, err := Render(card)
	if err != nil {
		return nil, err
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	var pix bytes.Buffer
	zw, err := zlib.NewWriterLevel(&pix, zlib.BestSpeed)
	if err != nil {
		return nil, err
	}
	row, prev := make([]byte, 1+3*w), make([]byte, 3*w)
	row[0] = 2 // PNG filter "Up"
	b := img.Bounds()
	for y := 0; y < h; y++ {
		off := img.PixOffset(b.Min.X, b.Min.Y+y)
		src := img.Pix[off : off+4*w]
		for x := 0; x < w; x++ {
			for ch := 0; ch < 3; ch++ {
				v := src[4*x+ch]
				row[1+3*x+ch] = v - prev[3*x+ch]
				prev[3*x+ch] = v
			}
		}
		if _, err := zw.Write(row); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	const mm = 72 / 25.4
	// The page follows the design's preset (PrintSizes): phone passes
	// centered on A6, badges and 5.5 x 8 cm cards at exactly their size.
	preset := Classic().Size.Preset
	if card.Design != nil {
		preset = card.Design.Size.Preset
	}
	ps, ok := PrintSizes[preset]
	if !ok {
		ps = PrintSizes["phone"]
	}
	pageW, pageH := ps.PageW*mm, ps.PageH*mm
	imgW, imgH, x, y := ps.ImgW*mm, ps.ImgH*mm, ps.X*mm, ps.Y*mm
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
	fmt.Fprintf(&out, "4 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode /DecodeParms << /Predictor 12 /Colors 3 /BitsPerComponent 8 /Columns %d >> /Length %d >>\nstream\n",
		w, h, w, pix.Len())
	out.Write(pix.Bytes())
	out.WriteString("\nendstream\nendobj\n")
	obj(5, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
	obj(6, "<< /Title ("+pdfString(card.EventName+" pass")+") /Producer (event-pass-platform) >>")
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
