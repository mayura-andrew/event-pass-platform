package passes

// Images in pass designs: uploads are checked, scaled and re-encoded here,
// and image elements are drawn from them.

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"regexp"
	"strings"
	"sync"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // accepted for upload, stored as PNG or JPEG
)

// Upload limits. Designs are at most 1200 wide or 1920 tall, so 2400 px on
// the long side keeps any image sharp at twice the pass's resolution.
const (
	MaxUploadBytes = 10 << 20
	maxUploadSide  = 8000
	maxStoredSide  = 2400
)

// Asset is an image ready to store.
type Asset struct {
	Data          []byte
	ContentType   string // image/png | image/jpeg
	Width, Height int
}

var ErrNotImage = errors.New("upload a PNG, JPEG or WebP image")

// PrepareUpload decodes an uploaded image, scales it down when it's larger
// than needed, and re-encodes it: JPEG when fully opaque, PNG otherwise.
// Re-encoding drops metadata (camera, location) and anything else hidden in
// the file.
func PrepareUpload(r io.Reader) (*Asset, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxUploadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxUploadBytes {
		return nil, fmt.Errorf("the image is over %d MB", MaxUploadBytes>>20)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg" && format != "webp") {
		return nil, ErrNotImage
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxUploadSide || cfg.Height > maxUploadSide {
		return nil, fmt.Errorf("the image is %d x %d; it can be at most %d px on each side", cfg.Width, cfg.Height, maxUploadSide)
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrNotImage
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if long := max(w, h); long > maxStoredSide {
		w, h = max(w*maxStoredSide/long, 1), max(h*maxStoredSide/long, 1)
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)

	var out bytes.Buffer
	a := &Asset{Width: w, Height: h}
	if dst.Opaque() {
		a.ContentType = "image/jpeg"
		err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 90})
	} else {
		a.ContentType = "image/png"
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, dst)
	}
	if err != nil {
		return nil, err
	}
	a.Data = out.Bytes()
	return a, nil
}

var assetSrcRe = regexp.MustCompile(`^asset:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// AssetID is the asset an image element shows ("asset:<id>"), or "".
func AssetID(src string) string {
	if m := assetSrcRe.FindStringSubmatch(src); m != nil {
		return m[1]
	}
	return ""
}

// AssetIDs lists the assets a design uses.
func AssetIDs(d Design) []string {
	var ids []string
	seen := map[string]bool{}
	for _, e := range d.Elements {
		if id := AssetID(e.Src); e.Type == "image" && id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// drawImage draws img into the w x h box at (x, y). cover fills the box and
// crops the overflow (centered), contain fits inside it, fill stretches.
func (c *canvas) drawImage(img image.Image, x, y, w, h int, fit string) {
	box := image.Rect(x, y, x+w, y+h)
	sb := img.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	switch fit {
	case "fill":
	case "contain":
		if sw*h > sh*w { // wider than the box
			nh := sh * w / sw
			box = image.Rect(x, y+(h-nh)/2, x+w, y+(h-nh)/2+nh)
		} else {
			nw := sw * h / sh
			box = image.Rect(x+(w-nw)/2, y, x+(w-nw)/2+nw, y+h)
		}
	default: // cover
		if sw*h > sh*w {
			cw := sh * w / h
			sb = image.Rect(sb.Min.X+(sw-cw)/2, sb.Min.Y, sb.Min.X+(sw-cw)/2+cw, sb.Max.Y)
		} else {
			ch := sw * h / w
			sb = image.Rect(sb.Min.X, sb.Min.Y+(sh-ch)/2, sb.Max.X, sb.Min.Y+(sh-ch)/2+ch)
		}
	}
	xdraw.CatmullRom.Scale(c.img, box, img, sb, xdraw.Over, nil)
}

// missingImage marks where an image couldn't be loaded: a light hatched box,
// so a pass still renders.
func (c *canvas) missingImage(x, y, w, h int) {
	c.rect(x, y, x+w, y+h, color.RGBA{0xEE, 0xF0, 0xF2, 0xFF})
	for i := 0; i < w+h; i += 24 {
		for t := 0; t < 2; t++ {
			for k := 0; k < min(w, h); k++ {
				px, py := x+i-k+t, y+k
				if px >= x && px < x+w && py < y+h {
					c.img.Set(px, py, color.RGBA{0xC9, 0xCE, 0xD4, 0xFF})
				}
			}
		}
	}
}

// ImageCache keeps decoded assets. Assets never change, so entries never go
// stale; the oldest are dropped past the limit.
type ImageCache struct {
	mu    sync.Mutex
	max   int
	order []string
	imgs  map[string]image.Image
}

func NewImageCache(max int) *ImageCache {
	return &ImageCache{max: max, imgs: map[string]image.Image{}}
}

func (c *ImageCache) Get(id string) (image.Image, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	img, ok := c.imgs[id]
	return img, ok
}

// Add decodes data and keeps it under id.
func (c *ImageCache) Add(id string, data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.imgs[id]; !ok {
		c.order = append(c.order, id)
		if len(c.order) > c.max {
			delete(c.imgs, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.imgs[id] = img
	return img, nil
}

// isImageFit says whether s is a known fit.
func isImageFit(s string) bool { return s == "" || strings.Contains("|cover|contain|fill|", "|"+s+"|") }

// AvatarSize is the side of a stored profile photo.
const AvatarSize = 400

// PrepareAvatar turns an uploaded photo into a square JPEG, cropped to the
// middle, re-encoded so nothing hidden in the file (such as location) is
// kept.
func PrepareAvatar(r io.Reader) ([]byte, error) {
	a, err := PrepareUpload(r)
	if err != nil {
		return nil, err
	}
	src, _, err := image.Decode(bytes.NewReader(a.Data))
	if err != nil {
		return nil, err
	}
	dst := image.NewRGBA(image.Rect(0, 0, AvatarSize, AvatarSize))
	// White under transparent pixels, then the photo cropped to a square.
	c := &canvas{img: dst}
	c.rect(0, 0, AvatarSize, AvatarSize, color.RGBA{0xFF, 0xFF, 0xFF, 0xFF})
	c.drawImage(src, 0, 0, AvatarSize, AvatarSize, "cover")
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
