package passes

// Pass designs: the JSON template format from the spec (pass_templates.design)
// and its renderer. The designer's live preview and every downloaded or
// emailed pass go through RenderDesign, so the preview is what participants
// get.

import (
	"embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"regexp"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// Design is a pass template.
type Design struct {
	Schema     int       `json:"schema"`
	Size       Size      `json:"size"`
	Background string    `json:"background"` // a color, see parseColor
	Elements   []Element `json:"elements"`

	// Images are the decoded assets image elements show, keyed by asset
	// ID, filled in by the caller before rendering. A missing one draws as
	// a placeholder.
	Images map[string]image.Image `json:"-"`
}

// Size presets: a phone screen and a 4 x 3 inch badge.
type Size struct {
	Preset string `json:"preset"` // phone | badge
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

var Presets = map[string]Size{
	"phone": {Preset: "phone", Width: 1080, Height: 1920},
	"badge": {Preset: "badge", Width: 1200, Height: 900},
}

// Element is one thing on the pass. Positions are in pixels from the top
// left of the design.
type Element struct {
	ID   string `json:"id"`
	Type string `json:"type"` // statusBand | text | answers | qr | rect | line | image
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w"`
	H    int    `json:"h"`

	Text        string   `json:"text,omitempty"` // with {{variables}}
	Font        Font     `json:"font"`
	Color       string   `json:"color,omitempty"`
	Align       string   `json:"align,omitempty"` // left | center | right
	MaxLines    int      `json:"maxLines,omitempty"`
	ShrinkToFit *Shrink  `json:"shrinkToFit,omitempty"`
	Transform   string   `json:"transform,omitempty"`   // upper
	HideIfEmpty string   `json:"hideIfEmpty,omitempty"` // a variable name
	VisibleFor  []string `json:"visibleFor,omitempty"`  // statuses; empty = all

	// QR codes.
	Size      int    `json:"size,omitempty"`
	FG        string `json:"fg,omitempty"`
	BG        string `json:"bg,omitempty"`
	QuietZone int    `json:"quietZone,omitempty"` // modules
	ECC       string `json:"ecc,omitempty"`       // L | M | Q | H

	// Images.
	Src string `json:"src,omitempty"` // "asset:<id>"
	Fit string `json:"fit,omitempty"` // cover (default) | contain | fill
}

type Font struct {
	Size   float64 `json:"size,omitempty"`
	Weight int     `json:"weight,omitempty"` // 400 or 700
}

type Shrink struct {
	Min float64 `json:"min"`
}

// Vars are the values a design can show.
type Vars struct {
	Values  map[string]string // org.name, event.name, participant.fullName, answers.<key>, ...
	Status  string            // registered | pending | waitlisted | cancelled
	Token   string            // QR content
	Answers []Line            // questions marked "printed on the pass"
}

// Variables lists the names a design may use in {{...}}; answers.<key> is
// also allowed for any question key.
var Variables = []string{"org.name", "event.name", "event.dateRange", "event.venue", "event.when",
	"participant.fullName", "participant.statusLabel", "pass.id"}

var placeholderRe = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)
var answerVarRe = regexp.MustCompile(`^answers\.[a-z][a-z0-9_]{0,59}$`)

func knownVariable(name string) bool {
	for _, v := range Variables {
		if v == name {
			return true
		}
	}
	return answerVarRe.MatchString(name)
}

func substitute(s string, vals map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		return vals[strings.TrimSpace(m[2:len(m)-2])]
	})
}

// StatusLabels are the words on the status band.
var StatusLabels = map[string]string{
	"registered": "REGISTERED", "pending": "AWAITING APPROVAL", "waitlisted": "WAITLIST", "cancelled": "CANCELLED",
}

// Design tokens usable as colors ("token:ink"). Status colors are fixed:
// a green band means the same at every event.
var tokens = map[string]color.RGBA{
	"ink": ink, "muted": muted, "surface": surface, "line": line, "primary": rgb(0x2350C8),
	"success": rgb(0x17804F), "warning": rgb(0x8A5509), "danger": rgb(0xB83A32),
}

// parseColor reads "#RRGGBB" or "token:<name>".
func parseColor(s string, def color.RGBA) (color.RGBA, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return def, nil
	case strings.HasPrefix(s, "token:"):
		c, ok := tokens[strings.TrimPrefix(s, "token:")]
		if !ok {
			return def, fmt.Errorf("unknown color token %q", s)
		}
		return c, nil
	case len(s) == 7 && s[0] == '#':
		v, err := strconv.ParseUint(s[1:], 16, 32)
		if err != nil {
			return def, fmt.Errorf("bad color %q", s)
		}
		return rgb(uint32(v)), nil
	}
	return def, fmt.Errorf("colors are #RRGGBB or token:name, not %q", s)
}

// RenderReport says what didn't fit, for the designer's scan checks.
type RenderReport struct {
	Overflow []string // element IDs whose text was cut short
}

// RenderDesign draws a pass from a design.
func RenderDesign(d Design, v Vars) (*image.RGBA, *RenderReport, error) {
	if err := loadFonts(); err != nil {
		return nil, nil, err
	}
	if d.Size.Width < 100 || d.Size.Height < 100 || d.Size.Width > 3000 || d.Size.Height > 3000 {
		return nil, nil, fmt.Errorf("design size %dx%d is out of range", d.Size.Width, d.Size.Height)
	}
	renderMu.Lock()
	defer renderMu.Unlock()

	rep := &RenderReport{}
	c := &canvas{img: image.NewRGBA(image.Rect(0, 0, d.Size.Width, d.Size.Height))}
	bg, err := parseColor(d.Background, surface)
	if err != nil {
		return nil, nil, err
	}
	c.rect(0, 0, d.Size.Width, d.Size.Height, bg)
	vals := map[string]string{}
	for k, x := range v.Values {
		vals[k] = x
	}
	vals["participant.statusLabel"] = StatusLabels[v.Status]
	if vals["participant.statusLabel"] == "" {
		vals["participant.statusLabel"] = StatusLabels["registered"]
	}

	for _, e := range d.Elements {
		if len(e.VisibleFor) > 0 && !contains(e.VisibleFor, v.Status) {
			continue
		}
		if e.HideIfEmpty != "" && strings.TrimSpace(vals[e.HideIfEmpty]) == "" {
			continue
		}
		col, err := parseColor(e.Color, ink)
		if err != nil {
			return nil, nil, err
		}
		fs := regular
		if e.Font.Weight >= 600 {
			fs = bold
		}
		size := e.Font.Size
		if size <= 0 {
			size = 40
		}
		min := size
		if e.ShrinkToFit != nil && e.ShrinkToFit.Min > 0 && e.ShrinkToFit.Min < size {
			min = e.ShrinkToFit.Min
		}
		maxLines := max(e.MaxLines, 1)

		switch e.Type {
		case "statusBand":
			band := bands[v.Status]
			if band.label == "" {
				band = bands["registered"]
			}
			c.rect(e.X, e.Y, e.X+e.W, e.Y+e.H, band.bg)
			label := substitute(orDefault(e.Text, "{{participant.statusLabel}}"), vals)
			lines, sz, _ := fit(label, bold, size, min, e.W-40, 1)
			top := e.Y + (e.H-int(sz*1.0))/2 - int(sz*0.08)
			drawAligned(c.img, lines, sz, surface, e.X+20, top, e.W-40, 1, "center")
		case "text":
			s := substitute(e.Text, vals)
			if e.Transform == "upper" {
				s = strings.ToUpper(s)
			}
			if strings.TrimSpace(s) == "" {
				continue
			}
			lines, sz, over := fit(s, fs, size, min, e.W, maxLines)
			if over {
				rep.Overflow = append(rep.Overflow, e.ID)
			}
			drawAligned(c.img, lines, sz, col, e.X, e.Y, e.W, 1.2, e.Align)
		case "answers":
			lh := int(size * 1.3)
			room := max(e.H/max(lh, 1), 1)
			for i, a := range v.Answers {
				if i >= room {
					rep.Overflow = append(rep.Overflow, e.ID)
					break
				}
				lines, sz, _ := fit(a.Label+": "+a.Value, fs, size, min, e.W, 1)
				drawAligned(c.img, lines, sz, col, e.X, e.Y+i*lh, e.W, 1.3, e.Align)
			}
		case "qr":
			fg, err := parseColor(e.FG, ink)
			if err != nil {
				return nil, nil, err
			}
			qbg, err := parseColor(e.BG, surface)
			if err != nil {
				return nil, nil, err
			}
			c.qrCode(v.Token, e.X, e.Y, e.Size, fg, qbg, max(e.QuietZone, 4), e.ECC)
		case "rect", "line":
			c.rect(e.X, e.Y, e.X+e.W, e.Y+max(e.H, 1), col)
		case "image":
			if img := d.Images[AssetID(e.Src)]; img != nil {
				c.drawImage(img, e.X, e.Y, e.W, e.H, e.Fit)
			} else {
				c.missingImage(e.X, e.Y, e.W, e.H)
			}
		default:
			return nil, nil, fmt.Errorf("unknown element type %q", e.Type)
		}
	}
	if c.err != nil {
		return nil, nil, c.err
	}
	return c.img, rep, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

var eccLevels = map[string]qrcode.RecoveryLevel{"L": qrcode.Low, "M": qrcode.Medium, "Q": qrcode.High, "H": qrcode.Highest}

// qrCode draws whole-pixel modules with the given quiet zone, centered in a
// size x size square of bg at (x, y).
func (c *canvas) qrCode(token string, x, y, size int, fg, bg color.RGBA, quiet int, ecc string) {
	level, ok := eccLevels[ecc]
	if !ok {
		level = qrcode.Medium
	}
	q, err := qrcode.New(token, level)
	if err != nil {
		c.err = err
		return
	}
	q.DisableBorder = true
	bm := q.Bitmap()
	n := len(bm) + 2*quiet
	module := size / n
	if module < 1 {
		c.err = fmt.Errorf("the QR code is too small to draw (%d px for %d modules)", size, n)
		return
	}
	off := (size-module*n)/2 + quiet*module
	c.rect(x, y, x+size, y+size, bg)
	for r, row := range bm {
		for col, dark := range row {
			if dark {
				px, py := x+off+col*module, y+off+r*module
				c.rect(px, py, px+module, py+module, fg)
			}
		}
	}
}

// QRModules is how many modules the token's QR code has without a border,
// for scan checks.
func QRModules(token, ecc string) (int, error) {
	level, ok := eccLevels[ecc]
	if !ok {
		level = qrcode.Medium
	}
	q, err := qrcode.New(token, level)
	if err != nil {
		return 0, err
	}
	q.DisableBorder = true
	return len(q.Bitmap()), nil
}

// ---- Gallery ----------------------------------------------------------------

//go:embed gallery/*.json
var galleryFS embed.FS

// GalleryTemplate is a built-in starting point.
type GalleryTemplate struct {
	Key, Name, Description string
	Design                 Design
}

// Gallery lists the built-in designs, Classic first.
func Gallery() []GalleryTemplate {
	var out []GalleryTemplate
	for _, g := range []struct{ key, name, desc string }{
		{"classic", "Classic status band", "The big colored status band, name and QR code: the current pass."},
		{"conference", "Conference badge", "A 4 x 3 inch badge for badge printers: name large, QR on the side."},
		{"minimal", "Minimal", "Name, QR and pass ID on white, no band."},
	} {
		b, err := galleryFS.ReadFile("gallery/" + g.key + ".json")
		if err != nil {
			panic(err)
		}
		var d Design
		if err := json.Unmarshal(b, &d); err != nil {
			panic(fmt.Sprintf("gallery %s: %v", g.key, err))
		}
		out = append(out, GalleryTemplate{Key: g.key, Name: g.name, Description: g.desc, Design: d})
	}
	return out
}

// Classic is the default design for events without a template.
func Classic() Design { return Gallery()[0].Design }

// ArtworkDesign starts a design from uploaded artwork: the image fills the
// pass, with a light panel for the name, the QR code and the pass ID on
// top, ready to be moved in the designer. Wide artwork makes a badge, tall
// artwork a phone pass. The status shows only when it isn't "registered",
// so a valid pass is the artwork as designed.
func ArtworkDesign(assetID string, width, height int) Design {
	src := "asset:" + assetID
	status := func(x, y, w int, align string) Element {
		return Element{ID: "status", Type: "text", Text: "{{participant.statusLabel}}", X: x, Y: y, W: w, H: 50,
			Font: Font{Size: 36, Weight: 700}, Color: "token:danger", Align: align,
			VisibleFor: []string{"pending", "waitlisted", "cancelled"}}
	}
	if width > height {
		return Design{Schema: 1, Size: Presets["badge"], Background: "token:surface", Elements: []Element{
			{ID: "artwork", Type: "image", Src: src, X: 0, Y: 0, W: 1200, H: 900, Fit: "cover"},
			{ID: "panel", Type: "rect", X: 40, Y: 560, W: 700, H: 300, Color: "token:surface"},
			{ID: "qrPanel", Type: "rect", X: 770, Y: 120, W: 400, H: 520, Color: "token:surface"},
			status(70, 580, 640, "left"),
			{ID: "name", Type: "text", Text: "{{participant.fullName}}", X: 70, Y: 635, W: 640, H: 200,
				Font: Font{Size: 80, Weight: 700}, Color: "token:ink", Align: "left", MaxLines: 2, ShrinkToFit: &Shrink{Min: 40}},
			{ID: "qr", Type: "qr", X: 785, Y: 135, Size: 370, FG: "#000000", BG: "#FFFFFF", QuietZone: 4, ECC: "M"},
			{ID: "passId", Type: "text", Text: "{{pass.id}}", X: 780, Y: 545, W: 380, H: 60,
				Font: Font{Size: 40, Weight: 700}, Color: "token:ink", Align: "center", ShrinkToFit: &Shrink{Min: 28}},
		}}
	}
	return Design{Schema: 1, Size: Presets["phone"], Background: "token:surface", Elements: []Element{
		{ID: "artwork", Type: "image", Src: src, X: 0, Y: 0, W: 1080, H: 1920, Fit: "cover"},
		{ID: "panel", Type: "rect", X: 90, Y: 760, W: 900, H: 1010, Color: "token:surface"},
		status(130, 790, 820, "center"),
		{ID: "name", Type: "text", Text: "{{participant.fullName}}", X: 130, Y: 845, W: 820, H: 200,
			Font: Font{Size: 84, Weight: 700}, Color: "token:ink", Align: "center", MaxLines: 2, ShrinkToFit: &Shrink{Min: 44}},
		{ID: "qr", Type: "qr", X: 240, Y: 1060, Size: 600, FG: "#000000", BG: "#FFFFFF", QuietZone: 4, ECC: "M"},
		{ID: "passId", Type: "text", Text: "{{pass.id}}", X: 130, Y: 1680, W: 820, H: 70,
			Font: Font{Size: 52, Weight: 700}, Color: "token:ink", Align: "center", ShrinkToFit: &Shrink{Min: 36}},
	}}
}
