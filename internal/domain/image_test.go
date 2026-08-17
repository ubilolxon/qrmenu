package domain

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeContentType(t *testing.T) {
	tests := map[string]string{
		"image/jpeg":                   "image/jpeg",
		"IMAGE/JPEG":                   "image/jpeg",
		"  image/png  ":                "image/png",
		"image/jpeg; charset=binary":   "image/jpeg",
		"image/webp;boundary=whatever": "image/webp",
		"":                             "",
	}

	for in, want := range tests {
		if got := NormalizeContentType(in); got != want {
			t.Errorf("NormalizeContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImageUploadValidate(t *testing.T) {
	tests := []struct {
		name    string
		in      ImageUpload
		wantErr bool
	}{
		{"jpeg", ImageUpload{"image/jpeg", 1024}, false},
		{"png", ImageUpload{"image/png", 1024}, false},
		{"webp", ImageUpload{"image/webp", 1024}, false},
		{"gif", ImageUpload{"image/gif", 1024}, false},
		{"case and parameters are tolerated", ImageUpload{"IMAGE/JPEG; charset=x", 1024}, false},
		{"exactly at the size limit", ImageUpload{"image/jpeg", MaxImageBytes}, false},

		// SVG is a document that can carry script, and the bucket is public.
		{"svg is refused", ImageUpload{"image/svg+xml", 1024}, true},
		{"pdf is refused", ImageUpload{"application/pdf", 1024}, true},
		{"html is refused", ImageUpload{"text/html", 1024}, true},
		{"empty type is refused", ImageUpload{"", 1024}, true},
		{"zero size", ImageUpload{"image/jpeg", 0}, true},
		{"negative size", ImageUpload{"image/jpeg", -1}, true},
		{"over the size limit", ImageUpload{"image/jpeg", MaxImageBytes + 1}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.in.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() accepted %+v", tt.in)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() rejected %+v: %v", tt.in, err)
			}
		})
	}
}

// The key is the tenancy boundary in object storage: one restaurant's prefix
// must never be reachable from another's id.
func TestStorageKey(t *testing.T) {
	rid := uuid.MustParse("018f0000-0000-7000-8000-000000000001")
	iid := uuid.MustParse("018f0000-0000-7000-8000-000000000002")

	got := StorageKey(rid, iid, "thumb.jpg")
	want := "restaurants/018f0000-0000-7000-8000-000000000001/images/018f0000-0000-7000-8000-000000000002/thumb.jpg"
	if got != want {
		t.Errorf("StorageKey() = %q, want %q", got, want)
	}

	// The empty name is the prefix used to delete an image, and it has to end
	// in a slash or a prefix delete would also match sibling ids.
	prefix := StorageKey(rid, iid, "")
	if !strings.HasSuffix(prefix, "/") {
		t.Errorf("the delete prefix %q does not end in a slash", prefix)
	}
	if !strings.HasPrefix(got, prefix) {
		t.Errorf("%q is not under the prefix %q", got, prefix)
	}
}

func readyImage() Image {
	w, h := int32(1600), int32(1200)
	ph := "#c8a27a"
	return Image{
		ID:          uuid.New(),
		Status:      ImageReady,
		Width:       &w,
		Height:      &h,
		Placeholder: &ph,
		Variants: map[string]ImageVariant{
			VariantThumb: {Key: "k/thumb.jpg", Width: 320, Height: 240},
			VariantCard:  {Key: "k/card.jpg", Width: 800, Height: 600},
			VariantFull:  {Key: "k/full.jpg", Width: 1600, Height: 1200},
		},
	}
}

func testURL(key string) string { return "https://cdn.test/" + key }

func TestToPublic(t *testing.T) {
	got := readyImage().ToPublic(testURL)
	if got == nil {
		t.Fatal("ToPublic returned nil for a complete, ready image")
	}

	if got.Thumb != "https://cdn.test/k/thumb.jpg" {
		t.Errorf("Thumb = %q", got.Thumb)
	}
	if got.Full != "https://cdn.test/k/full.jpg" {
		t.Errorf("Full = %q", got.Full)
	}
	if got.Width != 1600 || got.Height != 1200 {
		t.Errorf("size = %dx%d, want 1600x1200", got.Width, got.Height)
	}
	if got.Placeholder != "#c8a27a" {
		t.Errorf("Placeholder = %q", got.Placeholder)
	}
}

// Everything half-built must resolve to nil. A menu showing a broken image box
// is worse than a menu showing no image, and this is the single check standing
// between the two.
func TestToPublicRefusesIncompleteImages(t *testing.T) {
	tests := map[string]func(*Image){
		"still pending": func(i *Image) { i.Status = ImagePending },
		"failed":        func(i *Image) { i.Status = ImageFailed },
		"no width":      func(i *Image) { i.Width = nil },
		"no height":     func(i *Image) { i.Height = nil },
		"no variants":   func(i *Image) { i.Variants = nil },
		"missing thumb": func(i *Image) { delete(i.Variants, VariantThumb) },
		"missing card":  func(i *Image) { delete(i.Variants, VariantCard) },
		"missing full":  func(i *Image) { delete(i.Variants, VariantFull) },
		"blank key":     func(i *Image) { i.Variants[VariantCard] = ImageVariant{Key: ""} },
	}

	for name, breakIt := range tests {
		t.Run(name, func(t *testing.T) {
			img := readyImage()
			breakIt(&img)
			if got := img.ToPublic(testURL); got != nil {
				t.Errorf("ToPublic published an image that is %s: %+v", name, got)
			}
		})
	}
}

func TestAspectRatio(t *testing.T) {
	if got := (PublicImage{Width: 1600, Height: 1200}).AspectRatio(); got != 4.0/3.0 {
		t.Errorf("AspectRatio() = %v, want 4:3", got)
	}
	// Zero means "unknown", which the template reads as "do not reserve a box".
	for _, p := range []PublicImage{{}, {Width: 100}, {Height: 100}, {Width: -1, Height: 5}} {
		if got := p.AspectRatio(); got != 0 {
			t.Errorf("AspectRatio() = %v for %+v, want 0", got, p)
		}
	}
}

func TestImageExtension(t *testing.T) {
	tests := map[string]string{
		"image/jpeg":       ".jpg",
		"IMAGE/PNG":        ".png",
		"image/webp":       ".webp",
		"image/gif":        ".gif",
		"image/svg+xml":    "",
		"application/json": "",
	}
	for ct, want := range tests {
		if got := ImageExtension(ct); got != want {
			t.Errorf("ImageExtension(%q) = %q, want %q", ct, got, want)
		}
	}
}
