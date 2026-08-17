package imaging

import (
	"encoding/binary"
	"image"
	"image/draw"
)

// EXIF orientation, and why this file exists.
//
// A phone does not rotate the pixels when you turn it sideways. It stores the
// sensor's orientation as a tag and leaves the raster alone, so a portrait
// photo is a landscape bitmap plus a note saying "rotate this". Software that
// ignores the note shows every such photo on its side — which, for a menu of
// dish photos taken on a phone, is most of them.
//
// The tag is parsed here by hand rather than by pulling in an EXIF library:
// only one tag out of the whole standard is needed, and the parse is a fixed
// walk over a well-specified structure.

// OrientationNormal means the pixels are already the right way up.
const OrientationNormal = 1

const (
	markerPrefix = 0xFF
	markerAPP1   = 0xE1
	markerSOS    = 0xDA // start of scan: image data begins, no more metadata

	tagOrientation = 0x0112
	typeShort      = 3
)

// OrientationOf reads the EXIF orientation from a JPEG, returning
// OrientationNormal for anything it cannot read.
//
// Every failure is silent and falls back to "no rotation". A malformed or
// absent tag must never fail an upload: the photo is still a perfectly good
// photo, it just might be sideways.
func OrientationOf(jpegBytes []byte) int {
	b := jpegBytes
	if len(b) < 4 || b[0] != markerPrefix || b[1] != 0xD8 {
		return OrientationNormal
	}

	for i := 2; i+4 <= len(b); {
		if b[i] != markerPrefix {
			return OrientationNormal
		}
		marker := b[i+1]

		// Padding: a run of 0xFF bytes is legal between segments.
		if marker == markerPrefix {
			i++
			continue
		}
		// Standalone markers carry no length field.
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD9) {
			i += 2
			continue
		}
		if marker == markerSOS {
			return OrientationNormal
		}

		segLen := int(binary.BigEndian.Uint16(b[i+2:]))
		if segLen < 2 || i+2+segLen > len(b) {
			return OrientationNormal
		}
		if marker == markerAPP1 {
			if o, ok := orientationFromAPP1(b[i+4 : i+2+segLen]); ok {
				return o
			}
		}
		i += 2 + segLen
	}
	return OrientationNormal
}

const exifHeader = "Exif\x00\x00"

// orientationFromAPP1 walks the TIFF structure inside an APP1 segment.
func orientationFromAPP1(seg []byte) (int, bool) {
	if len(seg) < len(exifHeader)+8 || string(seg[:len(exifHeader)]) != exifHeader {
		return 0, false
	}
	tiff := seg[len(exifHeader):]

	var order binary.ByteOrder
	switch string(tiff[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, false
	}
	if order.Uint16(tiff[2:4]) != 42 { // the TIFF magic number
		return 0, false
	}

	// Offsets are counted from the start of the TIFF header, not the segment.
	ifdOffset := int(order.Uint32(tiff[4:8]))
	if ifdOffset < 8 || ifdOffset+2 > len(tiff) {
		return 0, false
	}

	count := int(order.Uint16(tiff[ifdOffset:]))
	entries := tiff[ifdOffset+2:]

	const entrySize = 12
	for k := 0; k < count; k++ {
		e := k * entrySize
		if e+entrySize > len(entries) {
			break
		}
		if order.Uint16(entries[e:]) != tagOrientation {
			continue
		}
		// A SHORT value is stored inline in the value field, left-aligned.
		if order.Uint16(entries[e+2:]) != typeShort {
			continue
		}
		v := int(order.Uint16(entries[e+8:]))
		if v >= 1 && v <= 8 {
			return v, true
		}
	}
	return 0, false
}

// ApplyOrientation returns the image rotated and flipped so it displays the way
// the camera intended.
//
// Orientations 5–8 swap width and height, which is why this has to run before
// anything reads the dimensions — the size stored on the image row and served
// to the page must be the size after correction, or every photo reserves a box
// of the wrong shape.
func ApplyOrientation(img image.Image, orientation int) image.Image {
	if orientation <= OrientationNormal || orientation > 8 {
		return img
	}

	src := toRGBA(img)
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	// Each case maps a destination pixel back to its source pixel. Reading
	// backwards from the destination means every output pixel is written
	// exactly once, with no gaps to fill.
	var dstW, dstH int
	var at func(x, y int) (int, int)

	switch orientation {
	case 2: // mirror horizontal
		dstW, dstH = w, h
		at = func(x, y int) (int, int) { return w - 1 - x, y }
	case 3: // rotate 180
		dstW, dstH = w, h
		at = func(x, y int) (int, int) { return w - 1 - x, h - 1 - y }
	case 4: // mirror vertical
		dstW, dstH = w, h
		at = func(x, y int) (int, int) { return x, h - 1 - y }
	case 5: // transpose
		dstW, dstH = h, w
		at = func(x, y int) (int, int) { return y, x }
	case 6: // rotate 90 clockwise — the common portrait-phone case
		dstW, dstH = h, w
		at = func(x, y int) (int, int) { return y, h - 1 - x }
	case 7: // transverse
		dstW, dstH = h, w
		at = func(x, y int) (int, int) { return w - 1 - y, h - 1 - x }
	case 8: // rotate 270 clockwise
		dstW, dstH = h, w
		at = func(x, y int) (int, int) { return w - 1 - y, x }
	default:
		return img
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := range dstH {
		for x := range dstW {
			sx, sy := at(x, y)
			di := dst.PixOffset(x, y)
			si := src.PixOffset(b.Min.X+sx, b.Min.Y+sy)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

// toRGBA gives direct pixel access. image.Decode returns YCbCr for a JPEG,
// which has no Pix slice to index.
func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	return out
}
