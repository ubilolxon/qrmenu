package imaging

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// jpegWithOrientation splices a minimal EXIF APP1 segment carrying just the
// orientation tag into a real JPEG, immediately after the SOI marker.
//
// Building the bytes by hand rather than checking in a fixture keeps the test
// honest about the structure the parser walks: if the layout below is wrong,
// the parser is being tested against the same misunderstanding, so the round
// trip through a real jpeg.Decode below is what anchors it.
func jpegWithOrientation(t *testing.T, orientation uint16, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 0, 255})
		}
	}
	var body bytes.Buffer
	if err := jpeg.Encode(&body, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	raw := body.Bytes()

	// TIFF header (big-endian) + one IFD entry + the null next-IFD offset.
	var tiff bytes.Buffer
	tiff.WriteString("MM")
	binary.Write(&tiff, binary.BigEndian, uint16(42))
	binary.Write(&tiff, binary.BigEndian, uint32(8)) // IFD0 starts right after
	binary.Write(&tiff, binary.BigEndian, uint16(1)) // one entry
	binary.Write(&tiff, binary.BigEndian, uint16(tagOrientation))
	binary.Write(&tiff, binary.BigEndian, uint16(typeShort))
	binary.Write(&tiff, binary.BigEndian, uint32(1))   // count
	binary.Write(&tiff, binary.BigEndian, orientation) // value, left-aligned
	binary.Write(&tiff, binary.BigEndian, uint16(0))   // padding
	binary.Write(&tiff, binary.BigEndian, uint32(0))   // no next IFD

	payload := append([]byte(exifHeader), tiff.Bytes()...)

	var out bytes.Buffer
	out.Write(raw[:2]) // SOI
	out.WriteByte(0xFF)
	out.WriteByte(markerAPP1)
	binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write(raw[2:])
	return out.Bytes()
}

func TestOrientationOf(t *testing.T) {
	for want := uint16(1); want <= 8; want++ {
		data := jpegWithOrientation(t, want, 40, 20)

		// The splice must not have broken the JPEG itself.
		if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
			t.Fatalf("orientation %d: the constructed JPEG does not decode: %v", want, err)
		}
		if got := OrientationOf(data); got != int(want) {
			t.Errorf("OrientationOf = %d, want %d", got, want)
		}
	}
}

// Anything unreadable must fall back to "no rotation". A malformed tag is not a
// reason to reject a photo — it is still a perfectly good photo.
func TestOrientationOfFallsBackToNormal(t *testing.T) {
	plain := solidJPEG(t, 40, 20, color.RGBA{1, 2, 3, 255})

	tests := map[string][]byte{
		"no exif at all":           plain,
		"not a jpeg":               solidPNG(t, 40, 20, color.White),
		"empty":                    {},
		"soi only":                 {0xFF, 0xD8},
		"truncated segment":        append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x40}, []byte("Exif")...),
		"bogus byte order":         bogusByteOrder(t),
		"orientation out of range": jpegWithOrientation(t, 99, 40, 20),
	}

	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if got := OrientationOf(data); got != OrientationNormal {
				t.Errorf("OrientationOf = %d, want %d", got, OrientationNormal)
			}
		})
	}
}

func bogusByteOrder(t *testing.T) []byte {
	t.Helper()
	data := jpegWithOrientation(t, 6, 40, 20)
	// The byte-order mark sits 4 bytes into the APP1 payload, past "Exif\0\0".
	i := bytes.Index(data, []byte(exifHeader))
	if i < 0 {
		t.Fatal("could not find the exif header in the constructed jpeg")
	}
	data[i+len(exifHeader)] = 'X'
	data[i+len(exifHeader)+1] = 'X'
	return data
}

// ApplyOrientation is tested by where a single marked pixel ends up, which is
// the only way to tell a rotation from its mirror image.
func TestApplyOrientation(t *testing.T) {
	// A 3x2 image with one white pixel at the top-left, everything else black.
	const w, h = 3, 2
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			src.Set(x, y, color.RGBA{0, 0, 0, 255})
		}
	}
	src.Set(0, 0, color.RGBA{255, 255, 255, 255})

	tests := []struct {
		orientation  int
		wantW, wantH int
		markX, markY int
		what         string
	}{
		{1, 3, 2, 0, 0, "unchanged"},
		{2, 3, 2, 2, 0, "mirrored horizontally"},
		{3, 3, 2, 2, 1, "rotated 180"},
		{4, 3, 2, 0, 1, "mirrored vertically"},
		{5, 2, 3, 0, 0, "transposed"},
		{6, 2, 3, 1, 0, "rotated 90 clockwise"},
		{7, 2, 3, 1, 2, "transverse"},
		{8, 2, 3, 0, 2, "rotated 270 clockwise"},
	}

	for _, tt := range tests {
		t.Run(tt.what, func(t *testing.T) {
			out := ApplyOrientation(src, tt.orientation)
			b := out.Bounds()

			if b.Dx() != tt.wantW || b.Dy() != tt.wantH {
				t.Fatalf("size = %dx%d, want %dx%d — orientations 5-8 swap the axes",
					b.Dx(), b.Dy(), tt.wantW, tt.wantH)
			}

			gotX, gotY := findWhite(out)
			if gotX != tt.markX || gotY != tt.markY {
				t.Errorf("the top-left pixel moved to (%d,%d), want (%d,%d) when %s",
					gotX, gotY, tt.markX, tt.markY, tt.what)
			}
		})
	}
}

// A rotation applied twice in opposite directions must return the original, so
// the transforms are inverses and not merely plausible-looking.
func TestApplyOrientationRoundTrips(t *testing.T) {
	const w, h = 5, 3
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			src.Set(x, y, color.RGBA{uint8(x * 40), uint8(y * 60), 7, 255})
		}
	}

	// 6 is 90° clockwise and 8 is 270°; together they are a full turn.
	back := ApplyOrientation(ApplyOrientation(src, 6), 8)
	b := back.Bounds()
	if b.Dx() != w || b.Dy() != h {
		t.Fatalf("round trip changed the size to %dx%d", b.Dx(), b.Dy())
	}
	for y := range h {
		for x := range w {
			if back.At(x, y) != src.At(x, y) {
				t.Fatalf("pixel (%d,%d) differs after rotating 90 then 270", x, y)
			}
		}
	}
}

func TestApplyOrientationIgnoresOutOfRange(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for _, o := range []int{0, -1, 9, 100} {
		if out := ApplyOrientation(src, o); out != image.Image(src) {
			t.Errorf("orientation %d should have been ignored", o)
		}
	}
}

// Decode must apply the correction, or the dimensions recorded on the image row
// describe a differently-shaped picture than the one that is served.
func TestDecodeAppliesOrientation(t *testing.T) {
	// 200 wide, 120 tall, tagged as needing a 90° rotation.
	data := jpegWithOrientation(t, 6, 200, 120)

	src, err := Decode(bytes.NewReader(data), defaultLimits())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if src.Width != 120 || src.Height != 200 {
		t.Errorf("size = %dx%d, want 120x200 — a rotated photo must report its "+
			"corrected shape, or the page reserves a box of the wrong aspect",
			src.Width, src.Height)
	}
}

func findWhite(img image.Image) (int, int) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r > 0x8000 && g > 0x8000 && bl > 0x8000 {
				return x - b.Min.X, y - b.Min.Y
			}
		}
	}
	return -1, -1
}
