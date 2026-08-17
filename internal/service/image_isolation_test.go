package service_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/service"
	"github.com/ubilolxon/qrmenu/internal/storage"
)

// The image service is exercised here without a live object store.
//
// Every method checks the tenant *before* it touches storage, which is the
// property being tested: a cross-tenant call must be refused without so much as
// a connection attempt. Presigning is offline signing, so reserving an upload
// works against an endpoint where nothing is listening — and if that ever stops
// being true, these tests hang, which is a loud enough failure.
func (h *harness) imageService(t *testing.T) *service.Image {
	t.Helper()

	files, err := storage.New(config.Storage{
		Endpoint:  "localhost:9",
		AccessKey: "test",
		SecretKey: "test",
		Bucket:    "qrmenu-test",
		Region:    "us-east-1",
		PublicURL: "http://example.test/qrmenu-test",
	})
	if err != nil {
		t.Fatalf("build storage client: %v", err)
	}
	return service.NewImage(postgres.NewStore(h.pool), files, slog.New(slog.DiscardHandler))
}

func TestImageTenantIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	images := h.imageService(t)

	aliceSession := h.tenant(t)
	bobSession := h.tenant(t)
	alice := aliceSession.User.TenantID
	bob := bobSession.User.TenantID

	aliceRestaurant := h.restaurant(t, alice)
	bobRestaurant := h.restaurant(t, bob)

	upload, err := images.CreateUpload(ctx, alice, aliceRestaurant.ID, domain.ImageUpload{
		ContentType: "image/jpeg", ByteSize: 1024,
	})
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}
	aliceImage := upload.Image.ID

	if upload.UploadURL == "" {
		t.Error("no presigned URL was returned")
	}
	// The key is what every cleanup path globs on, so it has to contain both ids.
	wantPrefix := domain.StorageKey(aliceRestaurant.ID, aliceImage, "")
	if got := upload.Image.StorageKey; len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("storage key %q is not under the image's own prefix %q", got, wantPrefix)
	}

	t.Run("bob cannot read alice's image", func(t *testing.T) {
		if _, err := images.Get(ctx, bob, aliceRestaurant.ID, aliceImage); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Get returned %v, want ErrNotFound", err)
		}
	})

	t.Run("bob cannot read it through his own restaurant either", func(t *testing.T) {
		if _, err := images.Get(ctx, bob, bobRestaurant.ID, aliceImage); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Get returned %v, want ErrNotFound", err)
		}
	})

	t.Run("bob cannot complete alice's upload", func(t *testing.T) {
		_, err := images.Complete(ctx, bob, aliceRestaurant.ID, aliceImage)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Complete returned %v, want ErrNotFound", err)
		}
	})

	t.Run("bob cannot delete alice's image", func(t *testing.T) {
		if err := images.Delete(ctx, bob, aliceRestaurant.ID, aliceImage); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Delete returned %v, want ErrNotFound", err)
		}
	})

	t.Run("bob cannot reserve an upload against alice's restaurant", func(t *testing.T) {
		_, err := images.CreateUpload(ctx, bob, aliceRestaurant.ID, domain.ImageUpload{
			ContentType: "image/jpeg", ByteSize: 1024,
		})
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("CreateUpload returned %v, want ErrNotFound", err)
		}
	})

	t.Run("bob's listing does not include alice's image", func(t *testing.T) {
		list, err := images.List(ctx, bob, bobRestaurant.ID, 100, 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, img := range list {
			if img.ID == aliceImage {
				t.Fatal("alice's image appeared in bob's listing")
			}
		}
	})

	t.Run("alice can still reach her own", func(t *testing.T) {
		got, err := images.Get(ctx, alice, aliceRestaurant.ID, aliceImage)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != domain.ImagePending {
			t.Errorf("status = %q, want pending — nothing has been uploaded yet", got.Status)
		}
	})
}

// The composite foreign key, not a service check, is what stops one restaurant
// attaching another's photo. This test is the proof that the constraint is
// actually doing that work, and that the failure surfaces as a clean error
// rather than a 500.
func TestItemCannotBorrowAnotherRestaurantsImage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	images := h.imageService(t)

	aliceSession := h.tenant(t)
	bobSession := h.tenant(t)
	alice := aliceSession.User.TenantID
	bob := bobSession.User.TenantID

	aliceRestaurant := h.restaurant(t, alice)
	bobTree := h.tree(t, bob)

	upload, err := images.CreateUpload(ctx, alice, aliceRestaurant.ID, domain.ImageUpload{
		ContentType: "image/jpeg", ByteSize: 1024,
	})
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}

	_, err = h.menus.UpdateItem(ctx, bob, bobTree.restaurantID, bobTree.item.ID, service.ItemPatch{
		ImageID: &upload.Image.ID,
	})
	if err == nil {
		t.Fatal("bob attached alice's image to his own dish")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error was %v; the foreign key violation should map to ErrNotFound, "+
			"since bob must not learn whether that image id exists", err)
	}

	// And an image id that exists nowhere behaves the same way.
	missing := uuid.New()
	if _, err := h.menus.UpdateItem(ctx, bob, bobTree.restaurantID, bobTree.item.ID,
		service.ItemPatch{ImageID: &missing}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("attaching a nonexistent image returned %v, want ErrNotFound", err)
	}
}

// Deleting a photo must not delete the dish. The foreign key is ON DELETE SET
// NULL with an explicit column list; without that list Postgres would try to
// null the whole key, including items.restaurant_id, and refuse the delete.
func TestDeletingAnImageKeepsTheItem(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	images := h.imageService(t)

	session := h.tenant(t)
	tenant := session.User.TenantID
	tr := h.tree(t, tenant)

	upload, err := images.CreateUpload(ctx, tenant, tr.restaurantID, domain.ImageUpload{
		ContentType: "image/jpeg", ByteSize: 1024,
	})
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}

	attached, err := h.menus.UpdateItem(ctx, tenant, tr.restaurantID, tr.item.ID,
		service.ItemPatch{ImageID: &upload.Image.ID})
	if err != nil {
		t.Fatalf("attach image: %v", err)
	}
	if attached.ImageID == nil || *attached.ImageID != upload.Image.ID {
		t.Fatalf("image was not attached: %v", attached.ImageID)
	}

	// The object store is unreachable in this harness, so the prefix removal
	// fails and is logged — Delete is written to carry on regardless, because
	// the row and its references are the part that must not be left dangling.
	if err := images.Delete(ctx, tenant, tr.restaurantID, upload.Image.ID); err != nil {
		t.Fatalf("delete image: %v", err)
	}

	after, err := h.menus.GetItem(ctx, tenant, tr.restaurantID, tr.item.ID)
	if err != nil {
		t.Fatalf("the dish did not survive its photo being deleted: %v", err)
	}
	if after.ImageID != nil {
		t.Errorf("image_id is still %v; the foreign key should have nulled it", after.ImageID)
	}
}
