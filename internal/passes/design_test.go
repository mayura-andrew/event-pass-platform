package passes

import (
	"bytes"
	"flag"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden pass images in testdata/")

func TestGalleryPassesChecks(t *testing.T) {
	for _, g := range Gallery() {
		if ps := Check(g.Design); len(ps) > 0 {
			t.Errorf("gallery %s fails its own scan checks: %+v", g.Key, ps)
		}
	}
}

func TestChecksCatchProblems(t *testing.T) {
	cases := map[string]func(*Design){
		"no QR":            func(d *Design) { d.Elements = d.Elements[:5] },
		"two QR codes":     func(d *Design) { d.Elements = append(d.Elements, *element(*d, "qr")) },
		"low contrast":     func(d *Design) { element(*d, "qr").FG = "#999999" },
		"light on dark":    func(d *Design) { q := element(*d, "qr"); q.FG, q.BG = "#FFFFFF", "#000000" },
		"thin quiet zone":  func(d *Design) { element(*d, "qr").QuietZone = 2 },
		"tiny QR":          func(d *Design) { element(*d, "qr").Size = 300 },
		"QR off the pass":  func(d *Design) { element(*d, "qr").X = 600 },
		"unknown variable": func(d *Design) { element(*d, "org").Text = "{{org.secret}}" },
		"name can't fit": func(d *Design) {
			n := element(*d, "name")
			n.W, n.MaxLines, n.ShrinkToFit = 300, 1, nil
		},
		"bad color":    func(d *Design) { element(*d, "event").Color = "blue" },
		"text over QR": func(d *Design) { element(*d, "name").Y = 880 },
		"box on top": func(d *Design) {
			d.Elements = append(d.Elements, Element{ID: "x", Type: "rect", X: 300, Y: 1000, W: 100, H: 100})
		},
		"bad preset":   func(d *Design) { d.Size.Preset = "poster" },
		"off the pass": func(d *Design) { element(*d, "when").Y = 1900 },
	}
	for name, mutate := range cases {
		d := Classic()
		mutate(&d)
		if ps := Check(d); len(ps) == 0 {
			t.Errorf("%s: no problem reported", name)
		}
	}
	// Answers variables are allowed for any question key.
	d := Classic()
	element(d, "org").Text = "Team {{answers.team_name}}"
	if ps := Check(d); len(ps) > 0 {
		t.Errorf("answers variable rejected: %+v", ps)
	}
}

// Snapshot tests (spec): each gallery design with three sample participants
// matches its golden image within 0.5% of pixels. Regenerate with
//
//	go test ./internal/passes -run Snapshots -update
func TestGallerySnapshots(t *testing.T) {
	kr := DerivedKeyRing([]byte("snapshot"))
	tok, _ := kr.Issue(org, ev, part, "CSA-7KQ4-M2XD")
	samples := Samples(tok)
	for _, g := range Gallery() {
		for _, key := range []string{"short", "long", "nonlatin"} {
			img, _, err := RenderDesign(g.Design, samples[key])
			if err != nil {
				t.Fatalf("%s/%s: %v", g.Key, key, err)
			}
			path := filepath.Join("testdata", g.Key+"-"+key+".png")
			if *update {
				var buf bytes.Buffer
				png.Encode(&buf, img)
				os.MkdirAll("testdata", 0o755)
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("%s: %v (run with -update to create it)", path, err)
			}
			want, err := png.Decode(f)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			if d := pixelDiff(img, want); d > 0.005 {
				t.Errorf("%s/%s differs from its snapshot in %.2f%% of pixels", g.Key, key, d*100)
			}
		}
	}
}

// pixelDiff is the share of pixels that differ noticeably.
func pixelDiff(a, b image.Image) float64 {
	if a.Bounds() != b.Bounds() {
		return 1
	}
	diff, total := 0, 0
	r := a.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			total++
			ar, ag, ab, _ := a.At(x, y).RGBA()
			br, bg, bb, _ := b.At(x, y).RGBA()
			d := absDiff(ar, br) + absDiff(ag, bg) + absDiff(ab, bb)
			if d > 3*0x1000 { // ignore tiny anti-aliasing changes
				diff++
			}
		}
	}
	return float64(diff) / float64(total)
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// The old Card API renders the Classic design.
func TestCardUsesClassic(t *testing.T) {
	c := card("Ann")
	a, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := RenderDesign(Classic(), CardVars(c))
	if pixelDiff(a, b) != 0 {
		t.Error("Render(card) differs from RenderDesign(Classic)")
	}
	if !strings.Contains(string(mustPDF(t, c)), "/MediaBox [0 0 297.638 419.528]") {
		t.Error("phone design PDF isn't A6")
	}
	d := Gallery()[1].Design
	c.Design = &d
	if !strings.Contains(string(mustPDF(t, c)), "/MediaBox [0 0 288.000 216.000]") {
		t.Error("badge PDF isn't 4 x 3 inches")
	}
}

func mustPDF(t *testing.T, c Card) []byte {
	b, err := PDF(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
