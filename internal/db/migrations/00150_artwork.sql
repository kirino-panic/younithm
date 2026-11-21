-- +goose Up

-- Artwork (internal/artwork). Images are downloaded once from a linked
-- provider and kept under <config>/artwork. release.artwork_path (00001) and
-- artist.image_path hold the file name inside that folder; NULL = no image.
ALTER TABLE artist ADD COLUMN image_path TEXT;

-- One row per entity the artwork worker has looked at, so a miss is not
-- retried on every sweep. outcome: 'found', 'none' (no provider has an image)
-- or 'error' (a download or provider call failed; retried sooner).
CREATE TABLE artwork_try (
    entity_type TEXT NOT NULL CHECK (entity_type IN ('artist', 'release')),
    entity_id   INTEGER NOT NULL,
    outcome     TEXT NOT NULL CHECK (outcome IN ('found', 'none', 'error')),
    tried_at    TEXT NOT NULL,
    PRIMARY KEY (entity_type, entity_id)
);

-- +goose Down

DROP TABLE artwork_try;
ALTER TABLE artist DROP COLUMN image_path;
