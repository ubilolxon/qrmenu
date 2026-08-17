package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// An image moves pending → ready, or pending → failed. It is never edited: a
// replacement is a new upload, so a URL that has ever been handed out keeps
// pointing at the same bytes and can be cached forever.
type ImageStatus string

const (
	ImagePending ImageStatus = "pending"
	ImageReady   ImageStatus = "ready"
	ImageFailed  ImageStatus = "failed"
)

type Image struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Status       ImageStatus
	StorageKey   string
	ContentType  string
	ByteSize     int64
	Width        *int32
	Height       *int32
	Placeholder  *string
	Variants     map[string]ImageVariant
	Error        *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	UploadedAt   *time.Time
}

// ImageVariant is one generated size.
type ImageVariant struct {
	Key    string `json:"key"`
	Width  int32  `json:"width"`
	Height int32  `json:"height"`
	Bytes  int64  `json:"bytes"`
}

// The three sizes every image is reduced to.
//
// Thumb is the list row on a phone, card is the same photo opened, full is the
// largest anyone will see on a tablet. There is no "original" in the public
// payload: a 12 MP phone photo is 4 MB, and serving that to a guest on
// restaurant wifi is how a menu becomes slower than a paper one.
const (
	VariantThumb = "thumb"
	VariantCard  = "card"
	VariantFull  = "full"
)

// VariantWidths is the generation order, narrowest first.
var VariantWidths = []struct {
	Name  string
	Width int
}{
	{VariantThumb, 320},
	{VariantCard, 800},
	{VariantFull, 1600},
}

// MaxImageBytes caps an upload. Phone cameras produce 3–8 MB, so 10 MiB accepts
// anything genuine while keeping the decode bounded — the server has to hold
// the decoded bitmap in memory, which is width × height × 4 bytes and much
// larger than the file.
const MaxImageBytes = 10 << 20

// MaxImagePixels bounds the decoded bitmap independently of the file size. A
// "decompression bomb" is a small file that expands to an enormous raster: a
// 20 000 × 20 000 PNG is a few hundred KB on disk and 1.6 GB decoded.
const MaxImagePixels = 40_000_000

// MinImageDimension rejects an upload too small to be worth attaching — almost
// always a mistake such as picking an icon instead of a photo.
const MinImageDimension = 100

// AllowedImageTypes is an allow-list, not a block-list. Anything not named here
// is refused, so a format the decoder does not understand can never reach it.
//
// SVG is deliberately absent: it is a document, it can carry script, and the
// bucket is served publicly.
var AllowedImageTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

func ImageExtension(contentType string) string {
	return AllowedImageTypes[NormalizeContentType(contentType)]
}

// NormalizeContentType strips parameters and case, so "IMAGE/JPEG; charset=x"
// matches the allow-list.
func NormalizeContentType(ct string) string {
	base, _, _ := strings.Cut(ct, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// ImageUpload is what a client asks for when it wants somewhere to put bytes.
type ImageUpload struct {
	ContentType string
	ByteSize    int64
}

func (in ImageUpload) Validate() error {
	var v Validator

	ct := NormalizeContentType(in.ContentType)
	if _, ok := AllowedImageTypes[ct]; !ok {
		v.Add("content_type", "must be one of %s", strings.Join(allowedTypeList(), ", "))
	}

	v.Check(in.ByteSize > 0, "byte_size", "is required")
	v.Check(in.ByteSize <= MaxImageBytes, "byte_size",
		"must be at most %d bytes", MaxImageBytes)

	return v.Err()
}

func allowedTypeList() []string {
	out := make([]string, 0, len(AllowedImageTypes))
	for ct := range AllowedImageTypes {
		out = append(out, ct)
	}
	slices.Sort(out)
	return out
}

// StorageKey is where an image's bytes live.
//
// The restaurant id is the first path segment so a whole account's media can be
// listed, mirrored, or removed with a single prefix operation. The image id is
// its own segment for the same reason: deleting one image is deleting one
// prefix, with no risk of a partial delete leaving orphan derivatives behind.
func StorageKey(restaurantID, imageID uuid.UUID, name string) string {
	return fmt.Sprintf("restaurants/%s/images/%s/%s", restaurantID, imageID, name)
}

// PublicImage is the image as a guest sees it: three URLs, the intrinsic size,
// and a placeholder colour. No storage keys, no status, no bucket.
type PublicImage struct {
	Thumb       string `json:"thumb"`
	Card        string `json:"card"`
	Full        string `json:"full"`
	Width       int32  `json:"width"`
	Height      int32  `json:"height"`
	Placeholder string `json:"placeholder"`
}

// AspectRatio is used by the page to reserve the right box before the photo
// arrives. Zero when unknown, which the template treats as "do not reserve".
func (p PublicImage) AspectRatio() float64 {
	if p.Width <= 0 || p.Height <= 0 {
		return 0
	}
	return float64(p.Width) / float64(p.Height)
}

// ToPublic renders an image for the guest payload, resolving every variant key
// against the public base URL. It returns nil unless the image is ready and has
// all three sizes — a half-built image must never reach a menu.
func (img Image) ToPublic(publicURL func(key string) string) *PublicImage {
	if img.Status != ImageReady || img.Width == nil || img.Height == nil {
		return nil
	}

	out := PublicImage{Width: *img.Width, Height: *img.Height}
	if img.Placeholder != nil {
		out.Placeholder = *img.Placeholder
	}

	for _, spec := range VariantWidths {
		v, ok := img.Variants[spec.Name]
		if !ok || v.Key == "" {
			return nil
		}
		switch spec.Name {
		case VariantThumb:
			out.Thumb = publicURL(v.Key)
		case VariantCard:
			out.Card = publicURL(v.Key)
		case VariantFull:
			out.Full = publicURL(v.Key)
		}
	}
	return &out
}
