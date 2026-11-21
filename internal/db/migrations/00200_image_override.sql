-- +goose Up

-- image_override is an image a person chose by hand (a file name under
-- <config>/artwork, like artwork_path and image_path). It shows over the image
-- the artwork worker fetched, which it leaves untouched; NULL goes back to it.
ALTER TABLE artist ADD COLUMN image_override TEXT;
ALTER TABLE release ADD COLUMN image_override TEXT;

-- +goose Down
ALTER TABLE artist DROP COLUMN image_override;
ALTER TABLE release DROP COLUMN image_override;
