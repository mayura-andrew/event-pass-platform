package httphandler

// Integration tests for uploaded pass artwork (M6): an organizer uploads
// their own design, it becomes a template with the name and QR on top, and
// participants' passes show it.

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
)

func artworkPNG(w, h int, c color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// upload posts a multipart form; the API takes the CSRF header, pages the
// csrf field.
func upload(t *testing.T, u *user, path string, fields map[string]string, file []byte) *http.Response {
	t.Helper()
	return uploadAs(t, u, path, "file", fields, file)
}

// uploadAs is upload with the file in the named field (none when nil).
func uploadAs(t *testing.T, u *user, path, field string, fields map[string]string, file []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	if !strings.HasPrefix(path, "/api/") {
		mw.WriteField(csrfField, u.csrf)
	}
	if file != nil {
		fw, _ := mw.CreateFormFile(field, "artwork.png")
		fw.Write(file)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", u.env.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(csrfHeader, u.csrf)
	res, err := u.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestArtworkTemplate(t *testing.T) {
	f := newCheckinFixture(t, 1)
	owner := f.owner
	orgPage := "/o/" + f.org.Slug + "/pass-templates"
	assetsAPI := "/api/v1/orgs/" + f.org.Slug + "/pass-assets"
	red := color.NRGBA{200, 30, 30, 255}

	// Upload the artwork: the designer opens on the new template.
	res := upload(t, owner, orgPage+"/upload", map[string]string{"name": "Community artwork"}, artworkPNG(1080, 1920, red))
	html := body(res)
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Request.URL.Path, "/pass-templates/") || !strings.Contains(html, "Your artwork is in") {
		t.Fatalf("upload artwork: HTTP %d at %s", res.StatusCode, res.Request.URL.Path)
	}
	tplID := res.Request.URL.Path[strings.LastIndex(res.Request.URL.Path, "/")+1:]
	_, b := owner.api("GET", "/api/v1/orgs/"+f.org.Slug+"/pass-templates/"+tplID, nil)
	var tpl domain.PassTemplate
	json.Unmarshal(b, &tpl)
	var d passes.Design
	json.Unmarshal(tpl.Design, &d)
	ids := passes.AssetIDs(d)
	if tpl.Name != "Community artwork" || d.Size.Preset != "phone" || len(ids) != 1 {
		t.Fatalf("template from artwork: %s %+v", tpl.Name, d.Size)
	}

	// The stored image: re-encoded, privately cacheable.
	res = owner.get(assetsAPI + "/" + ids[0])
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/jpeg" || !strings.Contains(res.Header.Get("Cache-Control"), "private") {
		t.Errorf("asset: HTTP %d %s %s", res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Cache-Control"))
	}
	res.Body.Close()

	// A participant's pass shows the artwork, with their QR code on top.
	if status, b := owner.api("PUT", f.api+"/pass", map[string]any{"template_id": tplID}); status != http.StatusNoContent {
		t.Fatalf("assign: HTTP %d %s", status, b)
	}
	evPage := "/o/" + f.org.Slug + "/events/" + f.ev.ID
	_, ctype, pass := getBytes(t, owner, evPage+"/participants/"+f.people[0].ID+"/pass.png")
	img, err := png.Decode(bytes.NewReader(pass))
	if err != nil || ctype != "image/png" {
		t.Fatalf("pass: %s %v", ctype, err)
	}
	if r, g, _, _ := img.At(10, 10).RGBA(); r>>8 < 180 || g>>8 > 60 {
		t.Errorf("pass corner isn't the artwork: %v", img.At(10, 10))
	}
	qr := d.Elements[0]
	for _, e := range d.Elements {
		if e.Type == "qr" {
			qr = e
		}
	}
	if r, g, b, _ := img.At(qr.X+5, qr.Y+5).RGBA(); r>>8 != 255 || g>>8 != 255 || b>>8 != 255 {
		t.Errorf("the QR code's quiet zone isn't white: %v", img.At(qr.X+5, qr.Y+5))
	}
	_, _, preview := getBytes(t, owner, evPage+"/pass/preview.png?participant="+f.people[0].ID)
	if !bytes.Equal(preview, pass) {
		t.Error("the preview differs from the downloaded pass")
	}

	// The designer's + Image: the same image uploaded again is stored once.
	status, b := postUpload(t, owner, assetsAPI, artworkPNG(1080, 1920, red))
	var a domain.PassAsset
	json.Unmarshal(b, &a)
	if status != http.StatusCreated || a.ID != ids[0] || a.Src != "asset:"+ids[0] {
		t.Errorf("re-upload: HTTP %d %s", status, b)
	}
	if status, b := postUpload(t, owner, assetsAPI, []byte("%PDF-1.7 not an image")); status != http.StatusBadRequest || !strings.Contains(string(b), "PNG") {
		t.Errorf("PDF upload: HTTP %d %s", status, b)
	}

	// Check-in staff can't upload.
	if status, _ := postUpload(t, f.phones[1], assetsAPI, artworkPNG(10, 10, red)); status != http.StatusNotFound && status != http.StatusForbidden {
		t.Errorf("staff upload: HTTP %d", status)
	}

	// Another org's image can't be borrowed by its ID.
	other := f.env.newBrowser(t, "other")
	other.signIn("")
	orgB := other.createOrg("other")
	_, b = postUpload(t, other, "/api/v1/orgs/"+orgB.Slug+"/pass-assets", artworkPNG(200, 200, color.NRGBA{0, 0, 255, 255}))
	var foreign domain.PassAsset
	json.Unmarshal(b, &foreign)
	if res := owner.get(assetsAPI + "/" + foreign.ID); res.StatusCode != http.StatusNotFound {
		t.Errorf("another org's asset: HTTP %d, want 404", res.StatusCode)
	}
	d.Elements = append([]passes.Element{{ID: "stolen", Type: "image", Src: foreign.Src, X: 0, Y: 0, W: 100, H: 100}}, d.Elements...)
	status, b = owner.api("PUT", "/api/v1/orgs/"+f.org.Slug+"/pass-templates/"+tplID, map[string]any{"name": "Community artwork", "design": d})
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(b), "wasn't uploaded to this organization") {
		t.Errorf("save with another org's image: HTTP %d %s", status, b)
	}
}

func postUpload(t *testing.T, u *user, path string, file []byte) (int, []byte) {
	t.Helper()
	res := upload(t, u, path, nil, file)
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}
