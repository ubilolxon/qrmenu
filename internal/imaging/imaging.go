// Package imaging decodes uploaded images and produces the fixed set of sizes
// the menu serves.
//
// It exists as its own package because it is the only place that treats an
// upload as untrusted input. Everything here assumes the bytes are hostile
// until they have decoded successfully within explicit limits.
package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	"golang.org/x/image/draw"

	// Registers the WebP decoder. There is no WebP *encoder* in x/image, which
	// is why derivatives come out as JPEG or PNG regardless of what went in.
	_ "golang.org/x/image/webp"
)

// ErrTooLarge and ErrNotAnImage are separated because they mean different
// things to the person who uploaded: one is "use a smaller file", the other is
// "that is not a photo".
var (
	ErrTooLarge   = errors.New("imaging: image is too large")
	ErrNotAnImage = errors.New("imaging: data does not decode as an image")
	ErrTooSmall   = errors.New("imaging: image is too small")
)

// Source is a decoded upload, ready to derive sizes from.
type Source struct {
	Image  image.Image
	Format string // as reported by image.Decode: jpeg, png, gif, webp
	Width  int
	Height int
}

// Limits bound what Decode will accept. Both matter: a small file can still
// decode to an enormous raster, and a large file is a problem before it ever
// reaches the decoder.
type Limits struct {
	MaxBytes     int64
	MaxPixels    int64
	MinDimension int
}

// Decode reads an upload and proves it is an image.
//
// The order is deliberate. The byte limit is enforced while reading, so an
// oversized upload is abandoned rather than buffered. DecodeConfig then reads
// only the header, which gives the dimensions for the pixel check *before* the
// full decode allocates width × height × 4 bytes — the check has to happen
// first, or the bomb has already gone off by the time we could reject it.
func Decode(r io.Reader, limits Limits) (*Source, error) {
	// One byte past the limit, so hitting the limit exactly is still valid and
	// exceeding it is detectable without reading the whole stream.
	raw, err := io.ReadAll(io.LimitReader(r, limits.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("imaging: read: %w", err)
	}
	if int64(len(raw)) > limits.MaxBytes {
		return nil, fmt.Errorf("%w: over %d bytes", ErrTooLarge, limits.MaxBytes)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrNotAnImage)
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotAnImage, err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > limits.MaxPixels {
		return nil, fmt.Errorf("%w: %dx%d exceeds %d pixels",
			ErrTooLarge, cfg.Width, cfg.Height, limits.MaxPixels)
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotAnImage, err)
	}

	// Phone cameras record the sensor orientation in EXIF rather than rotating
	// the pixels. Ignoring it is why uploaded food photos come out sideways.
	if format == "jpeg" {
		img = ApplyOrientation(img, OrientationOf(raw))
	}

	b := img.Bounds()
	if b.Dx() < limits.MinDimension || b.Dy() < limits.MinDimension {
		return nil, fmt.Errorf("%w: %dx%d, minimum is %d",
			ErrTooSmall, b.Dx(), b.Dy(), limits.MinDimension)
	}

	return &Source{Image: img, Format: format, Width: b.Dx(), Height: b.Dy()}, nil
}

// Derivative is one generated size, encoded and ready to store.
type Derivative struct {
	Name        string
	Width       int
	Height      int
	ContentType string
	Ext         string
	Data        []byte
}

// JPEGQuality trades file size against artefacts. 82 is the usual sweet spot
// for photographs: visually indistinguishable from 95 at roughly half the
// bytes, which on a menu of forty dishes is the difference between a fast page
// and a slow one.
const JPEGQuality = 82

// Derive renders the source at the given width, preserving aspect ratio.
//
// It never upscales. A restaurant that uploads a 400 px photo gets a 400 px
// "full" size rather than a blurry 1600 px one — the alternative wastes
// bandwidth to look worse.
func (s *Source) Derive(name string, width int) (Derivative, error) {
	w, h := s.Width, s.Height
	if width > 0 && s.Width > width {
		w = width
		h = int(float64(s.Height) * float64(width) / float64(s.Width))
		if h < 1 {
			h = 1
		}
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))

	// CatmullRom is the slowest scaler in x/image and the only one that holds
	// up when shrinking a 12 MP photo to a 320 px thumbnail; the cheaper
	// kernels alias badly at that ratio. This runs once per upload, not per
	// request, so the cost is paid in the wrong place to matter.
	draw.CatmullRom.Scale(dst, dst.Bounds(), s.Image, s.Image.Bounds(), draw.Src, nil)

	var buf bytes.Buffer
	d := Derivative{Name: name, Width: w, Height: h}

	// Transparency has to survive, or a logo with a cut-out background comes
	// back with black corners. Everything else becomes JPEG, which is several
	// times smaller than PNG for a photograph.
	if s.hasAlpha() {
		if err := png.Encode(&buf, dst); err != nil {
			return Derivative{}, fmt.Errorf("imaging: encode png: %w", err)
		}
		d.ContentType, d.Ext = "image/png", ".png"
	} else {
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: JPEGQuality}); err != nil {
			return Derivative{}, fmt.Errorf("imaging: encode jpeg: %w", err)
		}
		d.ContentType, d.Ext = "image/jpeg", ".jpg"
	}

	d.Data = buf.Bytes()
	return d, nil
}

// hasAlpha reports whether any pixel is actually translucent.
//
// The format alone is not the answer: most PNGs are fully opaque, and encoding
// those as PNG rather than JPEG would multiply a photo's size for nothing. The
// standard image types all implement Opaque(), which does exactly this scan.
func (s *Source) hasAlpha() bool {
	type opaquer interface{ Opaque() bool }
	if o, ok := s.Image.(opaquer); ok {
		return !o.Opaque()
	}
	// An unknown implementation is assumed to have alpha: encoding a photo as
	// PNG wastes bytes, but flattening a transparent logo onto black is a
	// visible defect.
	return true
}

// Placeholder returns the image's average colour as #rrggbb.
//
// It is the background the page paints behind a photo while the photo loads.
// One colour rather than a blurhash, because the public page runs no
// JavaScript and so has nothing to decode a blurhash with.
func (s *Source) Placeholder() string {
	// Averaging a downscaled copy rather than the original: a 12 MP scan is 12
	// million iterations for a value that only has to be approximately right.
	const sample = 16
	small := image.NewRGBA(image.Rect(0, 0, sample, sample))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), s.Image, s.Image.Bounds(), draw.Src, nil)

	var rSum, gSum, bSum uint64
	for i := 0; i < len(small.Pix); i += 4 {
		rSum += uint64(small.Pix[i])
		gSum += uint64(small.Pix[i+1])
		bSum += uint64(small.Pix[i+2])
	}

	n := uint64(sample * sample)
	return fmt.Sprintf("#%02x%02x%02x", rSum/n, gSum/n, bSum/n)
}

// Registered so image.Decode recognises them; the imports are otherwise unused.
var (
	_ = gif.Decode
	_ = png.Decode
	_ = jpeg.Decode
)
