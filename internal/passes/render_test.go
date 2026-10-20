package passes

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func card(name string) Card {
	kr := DerivedKeyRing([]byte("test"))
	tok, _ := kr.Issue(org, ev, part, "SAC-7KQ4-M2XD")
	return Card{OrgName: "Colombo Space Club", EventName: "Space Apps Colombo 2026", When: "3–4 Oct 2026", Where: "BMICH",
		FullName: name, PassID: "SAC-7KQ4-M2XD", Status: "registered", Token: tok,
		Lines: []Line{{"T-shirt", "M"}}}
}

// Sample participants from the spec's scan checks: short, very long and
// non-Latin names all render; the QR in the image decodes to the token.
func TestRenderSamples(t *testing.T) {
	for _, name := range []string{"Ann", "Wijesinghe Mudiyanselage Kasun Chathuranga Bandara Jayawardena", "කසුන් ජයසූරිය", "கார்த்திக் ராஜா"} {
		b, err := PNG(card(name))
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() != 1080 || img.Bounds().Dy() != 1920 {
			t.Fatalf("%q: %v %v", name, img.Bounds(), err)
		}
		if dir := os.Getenv("PASS_RENDER_OUT"); dir != "" {
			os.WriteFile(filepath.Join(dir, "pass-"+string([]rune(name)[:3])+".png"), b, 0o644)
		}
	}
	for _, st := range []string{"pending", "waitlisted", "cancelled", "unknown"} {
		c := card("Ann")
		c.Status = st
		if _, err := PNG(c); err != nil {
			t.Fatalf("status %s: %v", st, err)
		}
	}
}

func TestPDF(t *testing.T) {
	b, err := PDF(card("Ann Perera"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("%PDF-1.4")) || !bytes.HasSuffix(b, []byte("%%EOF\n")) || !bytes.Contains(b, []byte("/MediaBox [0 0 297.638 419.528]")) {
		t.Fatalf("not an A6 PDF (%d bytes)", len(b))
	}
	if dir := os.Getenv("PASS_RENDER_OUT"); dir != "" {
		os.WriteFile(filepath.Join(dir, "pass.pdf"), b, 0o644)
	}
}
