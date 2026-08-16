-- +goose Up
-- Foundation objects every later migration builds on.
--
-- Primary keys use uuidv7() (built into PostgreSQL 18): globally unique like a
-- UUID, but time-ordered, so inserts stay at the right edge of the B-tree
-- instead of scattering random writes across it the way uuidv4 does.

CREATE EXTENSION IF NOT EXISTS citext;

-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION set_updated_at() IS
    'Trigger function: stamps updated_at on every UPDATE. Attach per table.';

-- +goose Down
DROP FUNCTION IF EXISTS set_updated_at();
DROP EXTENSION IF EXISTS citext;
