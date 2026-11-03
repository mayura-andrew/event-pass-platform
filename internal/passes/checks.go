package passes

import (
	"fmt"
	"image/color"
	"math"
	"strings"
)

// Problem is a scan check that failed; it blocks saving the design.
type Problem struct {
	Element string `json:"element,omitempty"`
	Message string `json:"message"`
}

// Samples are the spec's preview participants: a short name, a very long
// one, a non-Latin one, and a cancelled pass.
func Samples(token string) map[string]Vars {
	base := func(name, status string) Vars {
		return Vars{Status: status, Token: token, Answers: []Line{{"T-shirt", "M"}, {"Team", "Orbiters"}},
			Values: map[string]string{"org.name": "Community Events", "event.name": "Example Event 2026",
				"event.dateRange": "3–4 Oct 2026", "event.venue": "BMICH", "event.when": "3–4 Oct 2026 · BMICH",
				"participant.fullName": name, "pass.id": "CSA-7KQ4-M2XD", "answers.t_shirt": "M", "answers.team_name": "Orbiters"}}
	}
	return map[string]Vars{
		"short":     base("Ann", "registered"),
		"long":      base("Wijesinghe Mudiyanselage Kasun Chathuranga Bandara", "registered"),
		"nonlatin":  base("කසුන් ජයසූරිය", "registered"),
		"cancelled": base("Ann Perera", "cancelled"),
	}
}

// SampleOrder is how samples are offered in the designer.
var SampleOrder = []string{"short", "long", "nonlatin", "cancelled"}

// Printed widths: phone designs print 78.75 mm wide on A6 (see PDF), badges
// 101.6 mm (4 inches).
var printedWidthMM = map[string]float64{"phone": 78.75, "badge": 101.6}

// Check runs the scan checks on a design: one QR code, dark on light with at
// least 7:1 contrast, a quiet zone of at least 4 modules, at least 30 mm when
// printed and 40% of a phone's width, everything inside the pass, known
// variables only, and every sample name fitting.
func Check(d Design) []Problem {
	var ps []Problem
	add := func(el, f string, a ...any) { ps = append(ps, Problem{Element: el, Message: fmt.Sprintf(f, a...)}) }
	if _, ok := Presets[d.Size.Preset]; !ok {
		add("", "the size must be phone or badge")
		return ps
	}
	if d.Size != Presets[d.Size.Preset] {
		add("", "a %s design is %dx%d", d.Size.Preset, Presets[d.Size.Preset].Width, Presets[d.Size.Preset].Height)
	}
	if _, err := parseColor(d.Background, surface); err != nil {
		add("", "%s", err)
	}
	if len(d.Elements) > 60 {
		add("", "a pass can have at most 60 elements")
	}
	qrs, ids := 0, map[string]bool{}
	for _, e := range d.Elements {
		if e.ID == "" || ids[e.ID] {
			add(e.ID, "every element needs its own id")
		}
		ids[e.ID] = true
		switch e.Type {
		case "image":
			if AssetID(e.Src) == "" {
				add(e.ID, "%s has no uploaded image", e.ID)
			}
			if !isImageFit(e.Fit) {
				add(e.ID, "%s: fit must be cover, contain or fill", e.ID)
			}
			fallthrough
		case "statusBand", "text", "answers", "rect", "line":
			if e.W <= 0 || (e.Type != "line" && e.H <= 0) {
				add(e.ID, "%s has no size", e.ID)
			}
		case "qr":
			qrs++
			checkQR(d, e, add)
			continue
		default:
			add(e.ID, "unknown element type %q", e.Type)
			continue
		}
		if e.X < 0 || e.Y < 0 || e.X+e.W > d.Size.Width || e.Y+max(e.H, 1) > d.Size.Height {
			add(e.ID, "%s goes outside the pass", e.ID)
		}
		if _, err := parseColor(e.Color, ink); err != nil {
			add(e.ID, "%s", err)
		}
		if e.Font.Size != 0 && (e.Font.Size < 12 || e.Font.Size > 200) {
			add(e.ID, "%s: font size must be 12 to 200", e.ID)
		}
		if e.MaxLines > 5 {
			add(e.ID, "%s: at most 5 lines", e.ID)
		}
		for _, m := range placeholderRe.FindAllStringSubmatch(e.Text, -1) {
			if !knownVariable(m[1]) {
				add(e.ID, "%s uses the unknown variable {{%s}}", e.ID, m[1])
			}
		}
		if e.HideIfEmpty != "" && !knownVariable(e.HideIfEmpty) {
			add(e.ID, "%s hides on the unknown variable %s", e.ID, e.HideIfEmpty)
		}
		for _, s := range e.VisibleFor {
			if StatusLabels[s] == "" {
				add(e.ID, "%s: unknown status %q", e.ID, s)
			}
		}
	}
	if qrs != 1 {
		add("", "a pass needs exactly one QR code (it has %d)", qrs)
	} else {
		checkQRClear(d, add)
	}
	if len(ps) > 0 {
		return ps // the render below needs a sound design
	}

	// Every sample name fits (cut-short names fail).
	samples := Samples(sampleToken)
	for _, key := range SampleOrder {
		_, rep, err := RenderDesign(d, samples[key])
		if err != nil {
			add("", "%s", err)
			break
		}
		for _, id := range rep.Overflow {
			if el := element(d, id); el != nil && strings.Contains(el.Text, "participant.fullName") {
				add(id, "the %s sample name doesn't fit in %s: make it wider, allow more lines or a smaller minimum size", key, id)
			}
		}
	}
	return ps
}

// sampleToken is as long as a real token, so module counts are realistic.
const sampleToken = "EP1.AQEBoVAAAAAAcACAAAAAAAAAoQGhUAAAAABwAIAAAAAAAADhAaFQAAAAAHAAgAAAAAAAAPFDU0EtN0tRNC1NMlhE.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0-P0BBQkNERUZHSElKS0xNTk9Q"

func element(d Design, id string) *Element {
	for i := range d.Elements {
		if d.Elements[i].ID == id {
			return &d.Elements[i]
		}
	}
	return nil
}

func checkQR(d Design, e Element, add func(string, string, ...any)) {
	fg, err1 := parseColor(e.FG, ink)
	bg, err2 := parseColor(e.BG, surface)
	if err1 != nil || err2 != nil {
		add(e.ID, "the QR colors must be #RRGGBB or token:name")
		return
	}
	if luminance(fg) >= luminance(bg) {
		add(e.ID, "the QR code must be dark modules on a light background")
	}
	if r := contrast(fg, bg); r < 7 {
		add(e.ID, "the QR code's contrast is %.1f:1; it needs at least 7:1", r)
	}
	if q := max(e.QuietZone, 4); e.QuietZone != 0 && e.QuietZone < 4 {
		add(e.ID, "the QR quiet zone is %d modules; it needs at least 4", q)
	} else if e.QuietZone < 0 {
		add(e.ID, "the QR quiet zone can't be negative")
	}
	if e.X < 0 || e.Y < 0 || e.X+e.Size > d.Size.Width || e.Y+e.Size > d.Size.Height {
		add(e.ID, "the QR code goes outside the pass")
	}
	if d.Size.Preset == "phone" && float64(e.Size) < 0.4*float64(d.Size.Width) {
		add(e.ID, "the QR code is %d%% of the screen width; it needs at least 40%%", e.Size*100/d.Size.Width)
	}
	if mm := float64(e.Size) / float64(d.Size.Width) * printedWidthMM[d.Size.Preset]; mm < 30 {
		add(e.ID, "the QR code prints %.0f mm wide; it needs at least 30 mm", mm)
	}
	if n, err := QRModules(sampleToken, e.ECC); err == nil && e.Size/(n+2*max(e.QuietZone, 4)) < 3 {
		add(e.ID, "the QR code is too small for its modules to scan reliably")
	}
}

// checkQRClear refuses anything that covers the QR code or is hidden by it:
// text over a code can make it unscannable. Boxes and lines drawn before
// the code (underneath it) are allowed, as frames, and so are images such
// as uploaded artwork: the code draws its own light square on top.
func checkQRClear(d Design, add func(string, string, ...any)) {
	var qr Element
	qrAt := 0
	for i, e := range d.Elements {
		if e.Type == "qr" {
			qr, qrAt = e, i
		}
	}
	for i, e := range d.Elements {
		if e.Type == "qr" {
			continue
		}
		overlaps := e.X < qr.X+qr.Size && qr.X < e.X+e.W && e.Y < qr.Y+qr.Size && qr.Y < e.Y+max(e.H, 1)
		if !overlaps || ((e.Type == "rect" || e.Type == "line" || e.Type == "image") && i < qrAt) {
			continue
		}
		add(e.ID, "%s overlaps the QR code: move it clear so the code scans and nothing is hidden", e.ID)
	}
}

func luminance(c color.RGBA) float64 {
	ch := func(v uint8) float64 {
		x := float64(v) / 255
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(c.R) + 0.7152*ch(c.G) + 0.0722*ch(c.B)
}

// contrast is the WCAG contrast ratio.
func contrast(a, b color.RGBA) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
