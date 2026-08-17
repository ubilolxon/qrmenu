-- +goose Up

-- Uploaded images, and the derivative sizes generated from each one.
--
-- A row is created *before* the bytes exist. The client is handed a presigned
-- URL and uploads straight to object storage, so the API never carries the
-- upload traffic; the row starts in 'pending' and is promoted to 'ready' only
-- once the server has fetched the object back and confirmed it really decodes
-- as an image. Trusting the content type the client declared would be a hole:
-- a presigned PUT does not enforce it, and the bucket is public-read, so an
-- HTML file stored under a .jpg key is stored XSS on our own origin.
--
-- 'failed' is kept rather than deleted so the client can be told *why* an
-- upload was rejected instead of finding the row simply gone.
CREATE TABLE images (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    restaurant_id uuid NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,

    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'ready', 'failed')),

    -- The key of the original upload. Derivatives live beside it under the
    -- same prefix, which is what makes deleting an image one prefix removal.
    storage_key  text NOT NULL,
    content_type text NOT NULL,
    byte_size    bigint NOT NULL DEFAULT 0 CHECK (byte_size >= 0),

    -- Intrinsic size of the original, known only after decoding. Serving these
    -- in the public payload lets the page reserve the right box before the
    -- image loads, which is most of avoiding layout shift.
    width  integer CHECK (width IS NULL OR width > 0),
    height integer CHECK (height IS NULL OR height > 0),

    -- Average colour of the image as #rrggbb, used as the placeholder behind a
    -- photo while it loads. One cheap value beats a blurhash here: no decoder
    -- on the client, and the page has no JavaScript to run one with.
    placeholder text CHECK (placeholder IS NULL OR placeholder ~ '^#[0-9a-f]{6}$'),

    -- Derivatives as {"thumb": {"key": ..., "width": ..., "height": ...}, ...}.
    -- JSONB rather than a child table: they are written once, always read as a
    -- set, and never queried individually.
    variants jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- Why a 'failed' row failed. User-facing text, so it must stay free of
    -- internal detail.
    error text,

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    uploaded_at timestamptz,

    -- Required so items and restaurants can reference (id, restaurant_id)
    -- compositely, exactly as the menu tree does.
    CONSTRAINT images_id_restaurant_uniq UNIQUE (id, restaurant_id),

    -- A ready image has been decoded, so its dimensions are known. This is the
    -- invariant the public payload relies on.
    CONSTRAINT images_ready_is_measured CHECK (
        status <> 'ready' OR (width IS NOT NULL AND height IS NOT NULL)
    )
);

CREATE UNIQUE INDEX images_storage_key_uniq ON images (storage_key);

CREATE INDEX images_restaurant_idx ON images (restaurant_id, created_at DESC);

-- Supports the sweep for uploads that were presigned and then abandoned —
-- a browser tab closed mid-upload leaves a pending row forever otherwise.
CREATE INDEX images_pending_idx ON images (created_at) WHERE status = 'pending';

CREATE TRIGGER images_set_updated_at
    BEFORE UPDATE ON images FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Attachment points.
--
-- ON DELETE SET NULL, not CASCADE: deleting a photo must not delete the dish.
-- The composite foreign key is what stops one restaurant attaching another's
-- image — the same guarantee the menu tree already gets from Postgres rather
-- than from a service-layer check.
--
-- The column list after SET NULL is load-bearing. A plain ON DELETE SET NULL on
-- a composite key nulls *every* column in the key, so deleting an image would
-- try to null items.restaurant_id, which is NOT NULL — and the delete would
-- fail outright. Naming the column (Postgres 15+) nulls only the reference.
ALTER TABLE items
    ADD COLUMN image_id uuid,
    ADD CONSTRAINT items_image_fk FOREIGN KEY (image_id, restaurant_id)
        REFERENCES images (id, restaurant_id) ON DELETE SET NULL (image_id);

CREATE INDEX items_image_idx ON items (image_id) WHERE image_id IS NOT NULL;

-- Same again, and here the column list matters even more: the second column of
-- this key is the restaurant's own primary key, so an unqualified SET NULL
-- would attempt to null the id of the row it is attached to.
ALTER TABLE restaurants
    ADD COLUMN logo_image_id  uuid,
    ADD COLUMN cover_image_id uuid,
    ADD CONSTRAINT restaurants_logo_fk FOREIGN KEY (logo_image_id, id)
        REFERENCES images (id, restaurant_id) ON DELETE SET NULL (logo_image_id),
    ADD CONSTRAINT restaurants_cover_fk FOREIGN KEY (cover_image_id, id)
        REFERENCES images (id, restaurant_id) ON DELETE SET NULL (cover_image_id);

-- +goose Down
ALTER TABLE restaurants
    DROP CONSTRAINT IF EXISTS restaurants_cover_fk,
    DROP CONSTRAINT IF EXISTS restaurants_logo_fk,
    DROP COLUMN IF EXISTS cover_image_id,
    DROP COLUMN IF EXISTS logo_image_id;

DROP INDEX IF EXISTS items_image_idx;
ALTER TABLE items
    DROP CONSTRAINT IF EXISTS items_image_fk,
    DROP COLUMN IF EXISTS image_id;

DROP TABLE IF EXISTS images;
