package imaging

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// This package is the only one that treats its input as hostile, so most of
// what is worth testing is refusal: the things that must not decode, and the
// limits that must hold whatever the file claims about itself.

func defaultLimits() Limits {
	return Limits{MaxBytes: 10 << 20, MaxPixels: 40_000_000, MinDimension: 100}
}

// solidPNG returns an opaque image of the given size.
func solidPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func solidJPEG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// transparentPNG has a genuinely translucent pixel, which is what decides
// whether derivatives come out as PNG rather than JPEG.
func transparentPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{200, 160, 120, 255})
		}
	}
	img.Set(0, 0, color.RGBA{0, 0, 0, 0})

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeAcceptsRealImages(t *testing.T) {
	gifBuf := &bytes.Buffer{}
	pal := image.NewPaletted(image.Rect(0, 0, 200, 150), color.Palette{color.Black, color.White})
	if err := gif.Encode(gifBuf, pal, nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}

	tests := map[string]struct {
		data   []byte
		format string
	}{
		"png":  {solidPNG(t, 200, 150, color.RGBA{10, 20, 30, 255}), "png"},
		"jpeg": {solidJPEG(t, 200, 150, color.RGBA{10, 20, 30, 255}), "jpeg"},
		"gif":  {gifBuf.Bytes(), "gif"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			src, err := Decode(bytes.NewReader(tt.data), defaultLimits())
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if src.Format != tt.format {
				t.Errorf("Format = %q, want %q", src.Format, tt.format)
			}
			if src.Width != 200 || src.Height != 150 {
				t.Errorf("size = %dx%d, want 200x150", src.Width, src.Height)
			}
		})
	}
}

// The point of decoding at all is that the declared content type is worthless.
// These are the shapes an upload takes when someone is trying it on.
func TestDecodeRejectsNonImages(t *testing.T) {
	tests := map[string][]byte{
		"empty":               {},
		"html":                []byte(`<html><script>alert(1)</script></html>`),
		"svg with script":     []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"plain text":          []byte("this is not a photograph"),
		"jpeg magic only":     {0xFF, 0xD8, 0xFF, 0xE0},
		"png header, no idat": append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...),
	}

	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(bytes.NewReader(data), defaultLimits()); err == nil {
				t.Fatal("Decode accepted something that is not an image")
			}
		})
	}
}

func TestDecodeEnforcesByteLimit(t *testing.T) {
	data := solidPNG(t, 400, 400, color.RGBA{1, 2, 3, 255})

	limits := defaultLimits()
	limits.MaxBytes = int64(len(data)) - 1
	if _, err := Decode(bytes.NewReader(data), limits); err == nil {
		t.Fatal("Decode accepted a file over the byte limit")
	}

	// Exactly at the limit must still be accepted, or the boundary is off by
	// one and a file of precisely the advertised size is rejected.
	limits.MaxBytes = int64(len(data))
	if _, err := Decode(bytes.NewReader(data), limits); err != nil {
		t.Fatalf("Decode rejected a file exactly at the limit: %v", err)
	}
}

// A decompression bomb is small on disk and enormous decoded. The check has to
// happen from the header, before the full decode allocates the raster — this
// test would still pass if the order were wrong, but the process would be
// carrying the allocation it is supposed to be refusing.
func TestDecodeRejectsPixelBomb(t *testing.T) {
	data := solidPNG(t, 3000, 3000, color.RGBA{0, 0, 0, 255})

	limits := defaultLimits()
	limits.MaxPixels = 1_000_000 // 3000x3000 is nine times this
	if _, err := Decode(bytes.NewReader(data), limits); err == nil {
		t.Fatal("Decode accepted an image over the pixel limit")
	}
	if len(data) > 1<<20 {
		t.Logf("note: the bomb was %d bytes on disk but 36 MB decoded", len(data))
	}
}

func TestDecodeRejectsTinyImages(t *testing.T) {
	_, err := Decode(bytes.NewReader(solidPNG(t, 20, 20, color.White)), defaultLimits())
	if err == nil {
		t.Fatal("Decode accepted a 20x20 image")
	}
	if !strings.Contains(err.Error(), "too small") {
		t.Errorf("error should say the image is too small, got %v", err)
	}
}

func TestDeriveNeverUpscales(t *testing.T) {
	src, err := Decode(bytes.NewReader(solidPNG(t, 400, 300, color.RGBA{9, 9, 9, 255})), defaultLimits())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	d, err := src.Derive("full", 1600)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if d.Width != 400 || d.Height != 300 {
		t.Errorf("a 400x300 source became %dx%d; small images must not be blown up",
			d.Width, d.Height)
	}
}

func TestDerivePreservesAspectRatio(t *testing.T) {
	src, err := Decode(bytes.NewReader(solidPNG(t, 1600, 900, color.RGBA{9, 9, 9, 255})), defaultLimits())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	d, err := src.Derive("thumb", 320)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if d.Width != 320 {
		t.Errorf("width = %d, want 320", d.Width)
	}
	if d.Height != 180 {
		t.Errorf("height = %d, want 180 (16:9 preserved)", d.Height)
	}

	// The bytes must actually be an image of that size, not merely labelled so.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(d.Data))
	if err != nil {
		t.Fatalf("the derivative does not decode: %v", err)
	}
	if cfg.Width != d.Width || cfg.Height != d.Height {
		t.Errorf("derivative reports %dx%d but decodes as %dx%d",
			d.Width, d.Height, cfg.Width, cfg.Height)
	}
	if format != "jpeg" {
		t.Errorf("an opaque photo encoded as %s, want jpeg", format)
	}
}

// Flattening a transparent logo onto black is a visible defect, and encoding
// every photo as PNG is a bandwidth one. The choice is made per image.
func TestDeriveKeepsTransparency(t *testing.T) {
	src, err := Decode(bytes.NewReader(transparentPNG(t, 400, 400)), defaultLimits())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	d, err := src.Derive("card", 200)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if d.ContentType != "image/png" {
		t.Errorf("a transparent image became %s; the transparency is lost", d.ContentType)
	}
	if d.Ext != ".png" {
		t.Errorf("extension = %q, want .png", d.Ext)
	}
}

func TestDeriveOpaquePNGBecomesJPEG(t *testing.T) {
	src, err := Decode(bytes.NewReader(solidPNG(t, 400, 400, color.RGBA{200, 100, 50, 255})), defaultLimits())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	d, err := src.Derive("card", 200)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if d.ContentType != "image/jpeg" {
		t.Errorf("an opaque PNG stayed %s; photos should become JPEG", d.ContentType)
	}
}

func TestPlaceholder(t *testing.T) {
	src, err := Decode(bytes.NewReader(solidPNG(t, 200, 200, color.RGBA{0xc8, 0xa2, 0x7a, 255})), defaultLimits())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	got := src.Placeholder()
	// The column is constrained to ^#[0-9a-f]{6}$, so the shape matters as much
	// as the value.
	if len(got) != 7 || got[0] != '#' {
		t.Fatalf("Placeholder() = %q, want #rrggbb", got)
	}
	if got != "#c8a27a" {
		t.Errorf("Placeholder() = %q, want #c8a27a for a solid image of that colour", got)
	}
}
