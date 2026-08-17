package handler

import (
	"net/http"
	"slices"
	"time"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/service"
	"github.com/ubilolxon/qrmenu/internal/storage"
)

type Image struct {
	svc   *service.Image
	files *storage.Client
}

func NewImage(svc *service.Image, files *storage.Client) Image {
	return Image{svc: svc, files: files}
}

// The response never mentions a bucket or a storage key. A client needs URLs it
// can put in an <img>, and nothing else; exposing keys would make the storage
// layout part of the API and impossible to change.
type imageResponse struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	ContentType string            `json:"content_type"`
	ByteSize    int64             `json:"byte_size"`
	Width       *int32            `json:"width"`
	Height      *int32            `json:"height"`
	Placeholder *string           `json:"placeholder"`
	URLs        map[string]string `json:"urls"`
	Error       *string           `json:"error"`
	CreatedAt   time.Time         `json:"created_at"`
	UploadedAt  *time.Time        `json:"uploaded_at"`
}

func (h Image) toResponse(img domain.Image) imageResponse {
	urls := map[string]string{}
	for name, v := range img.Variants {
		if v.Key != "" {
			urls[name] = h.files.PublicURL(v.Key)
		}
	}

	return imageResponse{
		ID:          img.ID.String(),
		Status:      string(img.Status),
		ContentType: img.ContentType,
		ByteSize:    img.ByteSize,
		Width:       img.Width,
		Height:      img.Height,
		Placeholder: img.Placeholder,
		URLs:        urls,
		Error:       img.Error,
		CreatedAt:   img.CreatedAt,
		UploadedAt:  img.UploadedAt,
	}
}

type imageUploadRequest struct {
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
}

// The upload response is the whole contract for step one: PUT the bytes to
// upload_url with exactly this Content-Type, then POST to the complete URL.
// The content type is echoed back because it is part of the signature — sending
// a different one makes the PUT fail with a signature error rather than an
// obviously related message.
type imageUploadResponse struct {
	Image       imageResponse `json:"image"`
	UploadURL   string        `json:"upload_url"`
	UploadWith  string        `json:"upload_with_content_type"`
	ExpiresAt   time.Time     `json:"expires_at"`
	CompleteURL string        `json:"complete_url"`
}

// CreateUpload hands out somewhere to put bytes.
func (h Image) CreateUpload(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	var req imageUploadRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	upload, err := h.svc.CreateUpload(r.Context(), tenantID, restaurantID, domain.ImageUpload{
		ContentType: req.ContentType,
		ByteSize:    req.ByteSize,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}

	httperr.JSON(w, http.StatusCreated, imageUploadResponse{
		Image:      h.toResponse(upload.Image),
		UploadURL:  upload.UploadURL,
		UploadWith: upload.Image.ContentType,
		ExpiresAt:  upload.ExpiresAt,
		CompleteURL: "/api/v1/admin/restaurants/" + restaurantID.String() +
			"/images/" + upload.Image.ID.String() + "/complete",
	})
}

// Complete publishes an upload once the bytes are in place.
//
// This is where the file is decoded, so it is the slowest endpoint in the admin
// API — a 12 MP photo takes a moment to resize three times. That cost belongs
// here, once per upload, rather than on every guest's menu request.
func (h Image) Complete(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "imageID", "image_id")
	if !ok {
		return
	}

	img, err := h.svc.Complete(r.Context(), tenantID, restaurantID, id)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, h.toResponse(img))
}

func (h Image) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "imageID", "image_id")
	if !ok {
		return
	}
	img, err := h.svc.Get(r.Context(), tenantID, restaurantID, id)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, h.toResponse(img))
}

func (h Image) List(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	limit := int32(atoiDefault(r.URL.Query().Get("limit"), service.ImagePageSize))
	offset := int32(atoiDefault(r.URL.Query().Get("offset"), 0))

	images, err := h.svc.List(r.Context(), tenantID, restaurantID, limit, offset)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"images": mapSlice(images, h.toResponse)})
}

// Delete removes the image and unlinks it from anything using it. The dish
// survives; only its photo goes.
func (h Image) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "imageID", "image_id")
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), tenantID, restaurantID, id); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}

// Limits describes the upload rules, so a client can validate a file before
// spending a phone's bandwidth on an upload that will be rejected.
func (h Image) Limits(w http.ResponseWriter, r *http.Request) {
	types := make([]string, 0, len(domain.AllowedImageTypes))
	for ct := range domain.AllowedImageTypes {
		types = append(types, ct)
	}
	slices.Sort(types)

	sizes := make([]map[string]any, 0, len(domain.VariantWidths))
	for _, spec := range domain.VariantWidths {
		sizes = append(sizes, map[string]any{"name": spec.Name, "width": spec.Width})
	}

	httperr.JSON(w, http.StatusOK, map[string]any{
		"max_bytes":          domain.MaxImageBytes,
		"max_pixels":         domain.MaxImagePixels,
		"min_dimension":      domain.MinImageDimension,
		"allowed_types":      types,
		"generated_sizes":    sizes,
		"upload_ttl_seconds": int(service.UploadTTL.Seconds()),
	})
}
