package passes

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func solid(w, h int, c color.Color) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func encodePNG(img image.Image) []byte {
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func TestPrepareUpload(t *testing.T) {
	// Opaque artwork is stored as JPEG; a logo with transparency as PNG.
	a, err := PrepareUpload(bytes.NewReader(encodePNG(solid(1080, 1920, color.NRGBA{200, 30, 30, 255}))))
	if err != nil || a.ContentType != "image/jpeg" || a.Width != 1080 || a.Height != 1920 {
		t.Fatalf("opaque: %+v %v", a, err)
	}
	logo := solid(300, 100, color.NRGBA{0, 0, 255, 255})
	logo.Set(0, 0, color.NRGBA{})
	if a, err := PrepareUpload(bytes.NewReader(encodePNG(logo))); err != nil || a.ContentType != "image/png" {
		t.Errorf("transparent: %+v %v", a, err)
	}
	// Large images are scaled down to 2400 px on the long side.
	if a, err := PrepareUpload(bytes.NewReader(encodePNG(solid(3000, 6000, color.White)))); err != nil || a.Width != 1200 || a.Height != 2400 {
		t.Errorf("large: %+v %v", a, err)
	}
	// JPEG in (with whatever metadata) is re-encoded, not copied.
	var j bytes.Buffer
	jpeg.Encode(&j, solid(400, 400, color.White), nil)
	in := append([]byte{}, j.Bytes()...)
	if a, err := PrepareUpload(bytes.NewReader(in)); err != nil || bytes.Equal(a.Data, in) {
		t.Errorf("jpeg wasn't re-encoded: %v", err)
	}
	for name, data := range map[string][]byte{
		"text":     []byte("<svg xmlns='http://www.w3.org/2000/svg'/>"),
		"pdf":      []byte("%PDF-1.4"),
		"too wide": encodePNG(solid(9000, 10, color.White)),
	} {
		if _, err := PrepareUpload(bytes.NewReader(data)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestArtworkDesign(t *testing.T) {
	const id = "01a16000-0000-7000-8000-00000000abcd"
	for _, size := range [][2]int{{1080, 1920}, {1200, 900}} {
		d := ArtworkDesign(id, size[0], size[1])
		if ps := Check(d); len(ps) > 0 {
			t.Errorf("artwork %v fails its checks: %v", size, ps)
		}
		if got := AssetIDs(d); len(got) != 1 || got[0] != id {
			t.Errorf("asset IDs: %v", got)
		}
		// With the image: the artwork shows where nothing covers it.
		d.Images = map[string]image.Image{id: solid(size[0], size[1], color.NRGBA{200, 30, 30, 255})}
		img, _, err := RenderDesign(d, Samples(sampleToken)["short"])
		if err != nil {
			t.Fatal(err)
		}
		if c := img.RGBAAt(5, 5); c.R < 180 || c.G > 60 {
			t.Errorf("artwork %v not drawn: corner is %v", size, c)
		}
		// The status shows only on passes that aren't valid.
		st := element(d, "status")
		inStatus := func(sample string) []byte {
			img, _, _ := RenderDesign(d, Samples(sampleToken)[sample])
			return img.SubImage(image.Rect(st.X, st.Y, st.X+st.W, st.Y+st.H)).(*image.RGBA).Pix
		}
		if short, cancelled := inStatus("short"), inStatus("cancelled"); bytes.Equal(short, cancelled) {
			t.Errorf("artwork %v: cancelled pass looks the same as a valid one", size)
		}
	}
}

func TestImageFits(t *testing.T) {
	// A wide red-and-blue image (left half red) in a square box.
	src := solid(200, 100, color.NRGBA{0, 0, 255, 255})
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			src.Set(x, y, color.NRGBA{255, 0, 0, 255})
		}
	}
	draw := func(fit string) *image.RGBA {
		c := &canvas{img: image.NewRGBA(image.Rect(0, 0, 100, 100))}
		c.rect(0, 0, 100, 100, color.RGBA{255, 255, 255, 255})
		c.drawImage(src, 0, 0, 100, 100, fit)
		return c.img
	}
	// cover crops the sides: the middle column is the red/blue boundary.
	if c := draw("cover").RGBAAt(10, 50); c.R < 200 {
		t.Errorf("cover: left is %v, want red", c)
	}
	// contain letterboxes: white above and below.
	if c := draw("contain").RGBAAt(50, 5); c != (color.RGBA{255, 255, 255, 255}) {
		t.Errorf("contain: top is %v, want white", c)
	}
	// fill stretches: no white anywhere.
	if c := draw("fill").RGBAAt(50, 5); c == (color.RGBA{255, 255, 255, 255}) {
		t.Error("fill left white")
	}
}

func TestImageChecks(t *testing.T) {
	d := Classic()
	d.Elements = append(d.Elements, Element{ID: "logo", Type: "image", Src: "https://example.com/x.png", X: 10, Y: 10, W: 100, H: 100})
	if ps := Check(d); len(ps) == 0 || !strings.Contains(ps[0].Message, "no uploaded image") {
		t.Errorf("external image accepted: %v", ps)
	}
	// An image on top of the QR code is refused; underneath it's fine.
	qr := element(Classic(), "qr")
	over := Classic()
	img := Element{ID: "logo", Type: "image", Src: "asset:01a16000-0000-7000-8000-00000000abcd", X: qr.X + 10, Y: qr.Y + 10, W: 100, H: 100}
	over.Elements = append(over.Elements, img)
	if ps := Check(over); len(ps) == 0 {
		t.Error("image over the QR code accepted")
	}
	under := Classic()
	under.Elements = append([]Element{img}, under.Elements...)
	if ps := Check(under); len(ps) > 0 {
		t.Errorf("image under the QR code refused: %v", ps)
	}
	// Without the image loaded, a placeholder is drawn instead of failing.
	if _, _, err := RenderDesign(under, Samples(sampleToken)["short"]); err != nil {
		t.Errorf("missing image: %v", err)
	}
}
