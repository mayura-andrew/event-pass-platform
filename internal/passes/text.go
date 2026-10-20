package passes

// Shaped text for passes. Names arrive in any script (Sinhala and Tamil are
// everyday in Sri Lanka), and those scripts need shaping, not glyph-by-glyph
// drawing, so text goes through go-text/typesetting (HarfBuzz in Go) with a
// font fallback chain: Atkinson Hyperlegible, then Noto Sans Sinhala, then
// Noto Sans Tamil.

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"sync"

	"github.com/go-text/render"
	"github.com/go-text/typesetting/di"
	"github.com/go-text/typesetting/font"
	ot "github.com/go-text/typesetting/font/opentype"
	"github.com/go-text/typesetting/language"
	"github.com/go-text/typesetting/shaping"
	"golang.org/x/image/math/fixed"
)

// fontSet is the fallback chain at one weight.
type fontSet []*font.Face

var (
	fontsOnce sync.Once
	fontErr   error
	regular   fontSet
	bold      fontSet
	// renderMu serialises rendering: faces cache glyphs and aren't safe
	// for concurrent use. A pass renders in tens of milliseconds.
	renderMu sync.Mutex
	shaper   shaping.HarfbuzzShaper
)

func loadFonts() error {
	fontsOnce.Do(func() {
		load := func(name string, weight float32) *font.Face {
			if fontErr != nil {
				return nil
			}
			b, err := fontFS.ReadFile("fonts/" + name)
			if err != nil {
				fontErr = err
				return nil
			}
			f, err := font.ParseTTF(bytes.NewReader(b))
			if err != nil {
				fontErr = err
				return nil
			}
			if weight > 0 { // variable fonts: pick the weight
				f.SetVariations([]font.Variation{{Tag: ot.MustNewTag("wght"), Value: weight}})
			}
			return f
		}
		regular = fontSet{load("AtkinsonHyperlegible-Regular.ttf", 0), load("NotoSansSinhala.ttf", 400), load("NotoSansTamil.ttf", 400)}
		bold = fontSet{load("AtkinsonHyperlegible-Bold.ttf", 0), load("NotoSansSinhala.ttf", 700), load("NotoSansTamil.ttf", 700)}
	})
	return fontErr
}

// shaped is one line of text, shaped into runs.
type shaped struct {
	runs  []shaping.Output
	width int
}

func toFixed(px float64) fixed.Int26_6 { return fixed.Int26_6(px * 64) }

// shape lays out s in the font set at size px.
func shape(s string, fs fontSet, size float64) shaped {
	runes := []rune(s)
	if len(runes) == 0 {
		return shaped{}
	}
	in := shaping.Input{Text: runes, RunStart: 0, RunEnd: len(runes), Direction: di.DirectionLTR,
		Size: toFixed(size), Language: language.NewLanguage("en")}
	var out shaped
	var adv fixed.Int26_6
	for _, part := range shaping.SplitByFontGlyphs(in, fs) {
		part.Script = scriptOf(runes[part.RunStart:part.RunEnd])
		run := shaper.Shape(part)
		out.runs = append(out.runs, run)
		adv += run.Advance
	}
	out.width = adv.Ceil()
	return out
}

// scriptOf is the first specific script in rs (Latin when only spaces,
// digits and punctuation).
func scriptOf(rs []rune) language.Script {
	for _, r := range rs {
		if sc := language.LookupScript(r); sc != language.Common && sc != language.Inherited && sc != language.Unknown {
			return sc
		}
	}
	return language.Latin
}

// fit returns s laid out in at most maxLines lines of width, at the largest
// size from max down to min that fits; if even min doesn't fit, the last
// line ends with "…". It returns the size used.
func fit(s string, fs fontSet, max, min float64, width, maxLines int) ([]shaped, float64) {
	for size := max; ; size -= 2 {
		if size < min {
			size = min
		}
		lines, ok := wrapShaped(s, fs, size, width, maxLines)
		if ok || size == min {
			if !ok {
				words := strings.Fields(s)
				// Shorten word by word, then rune by rune, until "…" fits.
				last := lastLineText(words, fs, size, width, maxLines)
				for r := []rune(last); len(r) > 1; r = r[:len(r)-1] {
					if l := shape(strings.TrimRight(string(r), " ")+"…", fs, size); l.width <= width {
						lines[len(lines)-1] = l
						break
					}
				}
			}
			return lines, size
		}
	}
}

// lastLineText is what remains for the last line after maxLines-1 full
// lines.
func lastLineText(words []string, fs fontSet, size float64, width, maxLines int) string {
	line, used := "", 1
	var rest []string
	for i, w := range words {
		try := strings.TrimSpace(line + " " + w)
		if shape(try, fs, size).width <= width || line == "" {
			line = try
			continue
		}
		if used == maxLines {
			rest = words[i:]
			break
		}
		used++
		line = w
	}
	return strings.TrimSpace(line + " " + strings.Join(rest, " "))
}

func wrapShaped(s string, fs fontSet, size float64, width, maxLines int) ([]shaped, bool) {
	words := strings.Fields(s)
	var lines []shaped
	cur := ""
	for _, w := range words {
		try := strings.TrimSpace(cur + " " + w)
		if shape(try, fs, size).width <= width {
			cur = try
			continue
		}
		if cur != "" {
			lines = append(lines, shape(cur, fs, size))
		}
		cur = w
	}
	lines = append(lines, shape(cur, fs, size))
	ok := len(lines) <= maxLines
	for _, l := range lines {
		if l.width > width {
			ok = false
		}
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	return lines, ok
}

// drawLines draws shaped lines centered in [x, x+width) from top y with the
// given line height (in multiples of size) and returns the y below them.
func drawLines(img *image.RGBA, lines []shaped, size float64, col color.Color, x, y, width int, lineHeight float64) int {
	r := &render.Renderer{FontSize: float32(size), PixScale: 1, Color: col}
	step := int(size * lineHeight)
	ascent := int(size * 0.92)
	for i, l := range lines {
		lx := x + (width-l.width)/2
		base := y + ascent + i*step
		for _, run := range l.runs {
			lx = r.DrawShapedRunAt(run, img, lx, base)
		}
	}
	return y + len(lines)*step
}
