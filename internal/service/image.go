package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/imaging"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
	"github.com/ubilolxon/qrmenu/internal/storage"
)

// Image owns the upload lifecycle.
//
// Uploads are two-step on purpose. The client asks for somewhere to put bytes,
// gets a presigned URL, and uploads straight to object storage; the API never
// carries the traffic. It then asks the API to complete the upload, and only
// then does the server touch the bytes — to prove they are an image and to
// generate the sizes the menu will actually serve.
//
// The completion step is not a formality. A presigned PUT enforces nothing
// about content: anyone holding a URL signed for image/jpeg can upload HTML,
// and the bucket is served publicly, so publishing without decoding would be
// stored XSS on our own origin. Deciding an upload is an image means decoding
// it.
type Image struct {
	store  *postgres.Store
	files  *storage.Client
	logger *slog.Logger
}

func NewImage(store *postgres.Store, files *storage.Client, logger *slog.Logger) *Image {
	return &Image{store: store, files: files, logger: logger}
}

// UploadTTL is how long a presigned URL stays valid. Long enough for a slow
// phone connection to finish a 10 MB photo, short enough that a URL leaked from
// a log or a browser history is not a lasting write capability.
const UploadTTL = 15 * time.Minute

// StaleUploadAge is when an uncompleted upload is considered abandoned. It is
// comfortably longer than UploadTTL, so the sweep can never race a client that
// is still legitimately uploading.
const StaleUploadAge = 2 * time.Hour

func (s *Image) assertRestaurant(ctx context.Context, tenantID, restaurantID uuid.UUID) error {
	_, err := s.store.GetRestaurant(ctx, gen.GetRestaurantParams{ID: restaurantID, TenantID: tenantID})
	if err != nil {
		return fmt.Errorf("restaurant: %w", postgres.MapError(err))
	}
	return nil
}

// Upload is what a client needs to start uploading: where to PUT, and the id to
// complete with afterwards.
type Upload struct {
	Image     domain.Image
	UploadURL string
	ExpiresAt time.Time
}

// CreateUpload reserves an image and returns a presigned URL for its bytes.
//
// The row is written before the object exists, which is what makes the id
// stable: the storage key contains it, so the client cannot influence where its
// bytes land. A client-chosen key would let one restaurant write over another's
// photo.
func (s *Image) CreateUpload(ctx context.Context, tenantID, restaurantID uuid.UUID, in domain.ImageUpload) (Upload, error) {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return Upload{}, err
	}
	if err := in.Validate(); err != nil {
		return Upload{}, err
	}

	contentType := domain.NormalizeContentType(in.ContentType)
	// The id is generated here, not by the database, because the storage key is
	// derived from it and has to be known before the insert. It is then passed
	// in explicitly — letting the column default apply would produce a second,
	// different id and leave the original object outside the prefix everything
	// else uses.
	id := uuid.Must(uuid.NewV7())
	key := domain.StorageKey(restaurantID, id, "original"+domain.ImageExtension(contentType))

	row, err := s.store.CreateImage(ctx, gen.CreateImageParams{
		ID:           id,
		RestaurantID: restaurantID,
		StorageKey:   key,
		ContentType:  contentType,
		ByteSize:     in.ByteSize,
	})
	if err != nil {
		return Upload{}, wrap("create image", err)
	}

	url, err := s.files.PresignPut(ctx, key, contentType, UploadTTL)
	if err != nil {
		// The row is left behind deliberately; the sweep will remove it. Rolling
		// back here would mean a delete on a path that is already failing.
		return Upload{}, fmt.Errorf("presign upload: %w", err)
	}

	return Upload{
		Image:     postgres.ToImage(row),
		UploadURL: url,
		ExpiresAt: time.Now().Add(UploadTTL),
	}, nil
}

// Complete verifies an uploaded object and publishes it.
//
// Everything expensive happens here rather than on the read path: decode once,
// resize once, and afterwards the menu serves static files from a CDN.
func (s *Image) Complete(ctx context.Context, tenantID, restaurantID, id uuid.UUID) (domain.Image, error) {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return domain.Image{}, err
	}

	row, err := s.store.GetImage(ctx, gen.GetImageParams{ID: id, RestaurantID: restaurantID})
	if err != nil {
		return domain.Image{}, wrap("get image", err)
	}
	current := postgres.ToImage(row)

	// Completing an already-published image is a no-op, so a client that
	// retries after a timeout gets success rather than a confusing error.
	if current.Status == domain.ImageReady {
		return current, nil
	}

	// Everything that cleans up — deleting an image, sweeping an abandoned
	// upload — works by removing this prefix. If the original ever landed
	// outside it, both would appear to succeed while leaving the object behind
	// forever, on a bill nobody is watching. It is worth one string comparison
	// to make that a loud failure instead of a silent leak.
	prefix := domain.StorageKey(restaurantID, id, "")
	if !strings.HasPrefix(current.StorageKey, prefix) {
		return domain.Image{}, fmt.Errorf(
			"image %s is stored at %q, outside its own prefix %q", id, current.StorageKey, prefix)
	}

	info, err := s.files.Stat(ctx, current.StorageKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return s.fail(ctx, current, "No file was uploaded to the URL that was issued.")
		}
		return domain.Image{}, fmt.Errorf("stat upload: %w", err)
	}
	// Size is checked before the object is fetched: a presigned PUT cannot cap
	// what the client sends, so this is the first point the real size is known.
	if info.Size > domain.MaxImageBytes {
		return s.fail(ctx, current,
			fmt.Sprintf("The file is %d bytes; the limit is %d.", info.Size, domain.MaxImageBytes))
	}

	obj, err := s.files.Get(ctx, current.StorageKey)
	if err != nil {
		return domain.Image{}, fmt.Errorf("fetch upload: %w", err)
	}
	defer func() { _ = obj.Close() }()

	source, err := imaging.Decode(obj, imaging.Limits{
		MaxBytes:     domain.MaxImageBytes,
		MaxPixels:    domain.MaxImagePixels,
		MinDimension: domain.MinImageDimension,
	})
	if err != nil {
		return s.fail(ctx, current, rejectionMessage(err))
	}

	variants, err := s.deriveAndStore(ctx, current, source)
	if err != nil {
		return domain.Image{}, err
	}

	encoded, err := json.Marshal(variants)
	if err != nil {
		return domain.Image{}, fmt.Errorf("encode variants: %w", err)
	}

	updated, err := s.store.MarkImageReady(ctx, gen.MarkImageReadyParams{
		ID:           id,
		RestaurantID: restaurantID,
		// The type recorded is the one the decoder actually recognised, not the
		// one the client claimed.
		ContentType: "image/" + source.Format,
		ByteSize:    info.Size,
		Width:       int32(source.Width),
		Height:      int32(source.Height),
		Placeholder: source.Placeholder(),
		Variants:    encoded,
	})
	if err != nil {
		return domain.Image{}, wrap("mark image ready", err)
	}

	// An image that is already attached to an item was, until this moment,
	// unpublishable — so its arrival changes the rendered menu and the cached
	// copy has to be superseded.
	s.bumpMenuVersion(ctx, tenantID, restaurantID)

	return postgres.ToImage(updated), nil
}

// deriveAndStore renders every size and writes it beside the original.
func (s *Image) deriveAndStore(ctx context.Context, img domain.Image, source *imaging.Source) (map[string]domain.ImageVariant, error) {
	variants := make(map[string]domain.ImageVariant, len(domain.VariantWidths))

	for _, spec := range domain.VariantWidths {
		d, err := source.Derive(spec.Name, spec.Width)
		if err != nil {
			return nil, fmt.Errorf("derive %s: %w", spec.Name, err)
		}

		key := domain.StorageKey(img.RestaurantID, img.ID, d.Name+d.Ext)
		if err := s.files.Put(ctx, key, bytes.NewReader(d.Data), int64(len(d.Data)), d.ContentType); err != nil {
			return nil, fmt.Errorf("store %s: %w", d.Name, err)
		}

		variants[spec.Name] = domain.ImageVariant{
			Key:    key,
			Width:  int32(d.Width),
			Height: int32(d.Height),
			Bytes:  int64(len(d.Data)),
		}
	}
	return variants, nil
}

// fail records why an upload was rejected and reports it as a validation error.
//
// The row is kept rather than deleted so the client can be told what was wrong
// with the file; a row that simply vanished would be indistinguishable from a
// bug on our side.
func (s *Image) fail(ctx context.Context, img domain.Image, reason string) (domain.Image, error) {
	if _, err := s.store.MarkImageFailed(ctx, gen.MarkImageFailedParams{
		ID: img.ID, RestaurantID: img.RestaurantID, Error: reason,
	}); err != nil {
		s.logger.ErrorContext(ctx, "could not record image failure",
			slog.String("image_id", img.ID.String()), slog.Any("error", err))
	}
	return domain.Image{}, &domain.ValidationError{Fields: []domain.FieldError{{
		Field: "file", Message: reason,
	}}}
}

// rejectionMessage turns a decode failure into something the person who
// uploaded can act on, without leaking decoder internals.
func rejectionMessage(err error) string {
	switch {
	case errors.Is(err, imaging.ErrTooLarge):
		return "The image is too large to process."
	case errors.Is(err, imaging.ErrTooSmall):
		return fmt.Sprintf("The image must be at least %d pixels on each side.", domain.MinImageDimension)
	case errors.Is(err, imaging.ErrNotAnImage):
		return "The uploaded file is not a readable image."
	default:
		return "The uploaded file could not be processed."
	}
}

func (s *Image) Get(ctx context.Context, tenantID, restaurantID, id uuid.UUID) (domain.Image, error) {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return domain.Image{}, err
	}
	row, err := s.store.GetImage(ctx, gen.GetImageParams{ID: id, RestaurantID: restaurantID})
	if err != nil {
		return domain.Image{}, wrap("get image", err)
	}
	return postgres.ToImage(row), nil
}

// ImagePageSize bounds a listing. The media library of a large restaurant is
// hundreds of photos, and an unbounded list is a slow page nobody asked for.
const ImagePageSize = 100

func (s *Image) List(ctx context.Context, tenantID, restaurantID uuid.UUID, limit, offset int32) ([]domain.Image, error) {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > ImagePageSize {
		limit = ImagePageSize
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.store.ListImages(ctx, gen.ListImagesParams{
		RestaurantID: restaurantID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, wrap("list images", err)
	}
	return postgres.ToImages(rows), nil
}

// Delete removes the row and every object under its prefix.
//
// Order matters: the row goes first. If the storage delete then fails, the
// result is an orphaned object — wasted space. Doing it the other way round
// would leave a row pointing at bytes that no longer exist, which is a broken
// image on a live menu. Cheap wrong beats visible wrong.
func (s *Image) Delete(ctx context.Context, tenantID, restaurantID, id uuid.UUID) error {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return err
	}

	if _, err := s.store.DeleteImage(ctx, gen.DeleteImageParams{ID: id, RestaurantID: restaurantID}); err != nil {
		return wrap("delete image", err)
	}

	// The foreign keys have just set image_id to NULL on anything referencing
	// this row, so the rendered menu changed.
	s.bumpMenuVersion(ctx, tenantID, restaurantID)

	if err := s.files.RemovePrefix(ctx, domain.StorageKey(restaurantID, id, "")); err != nil {
		s.logger.ErrorContext(ctx, "image row deleted but its objects remain",
			slog.String("image_id", id.String()), slog.Any("error", err))
	}
	return nil
}

// SweepStale removes uploads that were reserved and never completed.
//
// Without this every abandoned upload — a closed tab, a failed phone
// connection — leaves a pending row and possibly an orphaned object, forever,
// on a bill that grows monotonically. It returns the number removed so the
// caller can log it.
func (s *Image) SweepStale(ctx context.Context) (int, error) {
	rows, err := s.store.ListStalePendingImages(ctx, gen.ListStalePendingImagesParams{
		OlderThan: time.Now().Add(-StaleUploadAge),
		MaxRows:   ImagePageSize,
	})
	if err != nil {
		return 0, wrap("list stale images", err)
	}

	removed := 0
	for _, row := range rows {
		if err := s.files.RemovePrefix(ctx, domain.StorageKey(row.RestaurantID, row.ID, "")); err != nil {
			s.logger.WarnContext(ctx, "could not clear objects for a stale upload",
				slog.String("image_id", row.ID.String()), slog.Any("error", err))
			// Fall through and delete the row anyway: leaving it would mean
			// retrying this same failure on every sweep forever.
		}
		if _, err := s.store.DeleteImageByID(ctx, row.ID); err != nil {
			s.logger.WarnContext(ctx, "could not delete a stale upload row",
				slog.String("image_id", row.ID.String()), slog.Any("error", err))
			continue
		}
		removed++
	}
	return removed, nil
}

// bumpMenuVersion supersedes the cached public menu.
//
// A failure here is logged rather than returned: the image operation itself
// succeeded, and reporting an error would invite a retry that repeats work
// already done. The cost of the miss is a stale menu for up to the cache TTL.
func (s *Image) bumpMenuVersion(ctx context.Context, tenantID, restaurantID uuid.UUID) {
	if _, err := s.store.BumpMenuVersion(ctx, gen.BumpMenuVersionParams{
		ID: restaurantID, TenantID: tenantID,
	}); err != nil {
		s.logger.ErrorContext(ctx, "could not bump menu version after an image change",
			slog.String("restaurant_id", restaurantID.String()), slog.Any("error", err))
	}
}
