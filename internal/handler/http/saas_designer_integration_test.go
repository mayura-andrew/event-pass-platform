package httphandler

// Integration tests for M5's pass designer. The gate: an admin builds a
// custom pass in the designer; the downloaded PNG and PDF match the preview.

import (
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
)

func getBytes(t *testing.T, u *user, path string) (int, string, []byte) {
	t.Helper()
	res := u.get(path)
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), b
}

func postBytes(t *testing.T, u *user, path string, payload any) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", u.env.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, u.csrf)
	res, err := u.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func TestDesignerGate(t *testing.T) {
	f := newCheckinFixture(t, 2)
	owner := f.owner
	base := "/api/v1/orgs/" + f.org.Slug + "/pass-templates"

	// Start from the gallery's badge, then make it ours.
	status, b := owner.api("POST", base, map[string]string{"from": "conference", "name": "Community badge"})
	var tpl domain.PassTemplate
	json.Unmarshal(b, &tpl)
	if status != http.StatusCreated || tpl.CurrentVersion != 1 {
		t.Fatalf("create from gallery: HTTP %d %s", status, b)
	}
	if res := owner.get("/o/" + f.org.Slug + "/pass-templates/" + tpl.ID); res.StatusCode != http.StatusOK {
		t.Fatalf("designer page: HTTP %d", res.StatusCode)
	}
	var d passes.Design
	json.Unmarshal(tpl.Design, &d)

	// A design that fails the scan checks is refused, with the reasons.
	bad := d
	bad.Elements = append([]passes.Element{}, d.Elements...)
	for i := range bad.Elements {
		if bad.Elements[i].Type == "qr" {
			bad.Elements[i].FG = "#AAAAAA"
		}
	}
	status, b = owner.api("PUT", base+"/"+tpl.ID, map[string]any{"name": "Community badge", "design": bad})
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(b), "contrast") {
		t.Fatalf("low-contrast QR: HTTP %d %s; want 422 with the contrast problem", status, b)
	}
	status, b = postBytes(t, owner, base+"/"+tpl.ID+"/check", map[string]any{"design": bad})
	if status != http.StatusOK || !strings.Contains(string(b), "contrast") {
		t.Errorf("check endpoint: %d %s", status, b)
	}

	// A good change: a team line printed from an answer, saved as version 2.
	good := d
	good.Elements = append(append([]passes.Element{}, d.Elements...), passes.Element{ID: "team", Type: "text",
		Text: "Food: {{answers.food}}", X: 60, Y: 720, W: 680, H: 50, Font: passes.Font{Size: 34, Weight: 700},
		Color: "token:primary", Align: "left", HideIfEmpty: "answers.food"})
	status, b = owner.api("PUT", base+"/"+tpl.ID, map[string]any{"name": "Community badge", "design": good})
	if status != http.StatusOK || !strings.Contains(string(b), `"version":2`) {
		t.Fatalf("save: HTTP %d %s", status, b)
	}

	// The preview is a PNG of the design.
	for _, sample := range []string{"short", "long", "nonlatin", "cancelled"} {
		status, b := postBytes(t, owner, base+"/"+tpl.ID+"/preview?sample="+sample, map[string]any{"design": good})
		if status != http.StatusOK || !bytes.HasPrefix(b, []byte("\x89PNG")) {
			t.Fatalf("preview %s: HTTP %d", sample, status)
		}
	}

	// The event uses version 2; a participant's download equals the preview.
	if status, b := owner.api("PUT", f.api+"/pass", map[string]any{"template_id": tpl.ID}); status != http.StatusNoContent {
		t.Fatalf("assign template: HTTP %d %s", status, b)
	}
	p := f.people[0]
	evPage := "/o/" + f.org.Slug + "/events/" + f.ev.ID
	status, _, preview := getBytes(t, owner, evPage+"/pass/preview.png?participant="+p.ID)
	_, ctype, download := getBytes(t, owner, evPage+"/participants/"+p.ID+"/pass.png")
	if status != http.StatusOK || ctype != "image/png" || !bytes.Equal(preview, download) {
		t.Fatalf("downloaded PNG (%d bytes) differs from the preview (%d bytes)", len(download), len(preview))
	}
	img, _ := png.Decode(bytes.NewReader(download))
	if img.Bounds().Dx() != 1200 || img.Bounds().Dy() != 900 {
		t.Errorf("badge pass is %v, want 1200x900", img.Bounds())
	}
	_, _, pdf := getBytes(t, owner, evPage+"/participants/"+p.ID+"/pass.pdf")
	if d := pdfImageDiff(t, pdf, img); d > 0.005 {
		t.Errorf("the PDF's image differs from the preview in %.2f%% of pixels", d*100)
	}

	// Editing the template again doesn't touch the event until it moves.
	moved := good
	moved.Background = "#F4F6FA"
	owner.api("PUT", base+"/"+tpl.ID, map[string]any{"name": "Community badge", "design": moved})
	_, _, again := getBytes(t, owner, evPage+"/participants/"+p.ID+"/pass.png")
	if !bytes.Equal(again, download) {
		t.Error("saving version 3 changed a pass pinned to version 2")
	}
	_, b = owner.api("GET", f.api+"/pass", nil)
	if !strings.Contains(string(b), `"version":2`) {
		t.Errorf("event pass: %s, want version 2", b)
	}

	// Re-send queues one email per registered participant.
	status, b = owner.api("POST", f.api+"/pass/resend", nil)
	if status != http.StatusOK || !strings.Contains(string(b), `"queued":2`) {
		t.Errorf("resend: HTTP %d %s", status, b)
	}

	// Check-in staff can't design or assign; the event pages render.
	staff := f.phones[1]
	if status, _ := staff.api("PUT", f.api+"/pass", map[string]any{"template_id": ""}); status != http.StatusForbidden {
		t.Errorf("check-in staff assigning a design: HTTP %d, want 403", status)
	}
	if status, _ := staff.api("GET", base, nil); status != http.StatusForbidden {
		t.Errorf("check-in staff (a member) listing templates: HTTP %d", status)
	}
	for _, path := range []string{evPage + "/pass", "/o/" + f.org.Slug + "/pass-templates"} {
		if res := owner.get(path); res.StatusCode != http.StatusOK {
			t.Errorf("%s: HTTP %d", path, res.StatusCode)
		}
	}
	// Back to Classic.
	owner.api("PUT", f.api+"/pass", map[string]any{"template_id": ""})
	_, _, classic := getBytes(t, owner, evPage+"/participants/"+p.ID+"/pass.png")
	ci, _ := png.Decode(bytes.NewReader(classic))
	if ci.Bounds().Dy() != 1920 {
		t.Error("back to Classic didn't render the phone design")
	}
}

// pdfImageDiff extracts the JPEG in a pass PDF and compares it with want.
func pdfImageDiff(t *testing.T, pdf []byte, want image.Image) float64 {
	t.Helper()
	start := bytes.Index(pdf, []byte("stream\n"))
	end := bytes.Index(pdf, []byte("\nendstream"))
	if start < 0 || end < start {
		t.Fatal("no image stream in the PDF")
	}
	got, err := jpeg.Decode(bytes.NewReader(pdf[start+7 : end]))
	if err != nil {
		t.Fatal(err)
	}
	if got.Bounds() != want.Bounds() {
		return 1
	}
	diff, total := 0, 0
	r := want.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y += 2 {
		for x := r.Min.X; x < r.Max.X; x += 2 {
			total++
			a1, a2, a3, _ := got.At(x, y).RGBA()
			b1, b2, b3, _ := want.At(x, y).RGBA()
			if absd(a1, b1)+absd(a2, b2)+absd(a3, b3) > 3*0x2000 { // JPEG noise allowed
				diff++
			}
		}
	}
	return float64(diff) / float64(total)
}

func absd(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}
