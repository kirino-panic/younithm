-- +goose Up
-- Catalogue artist ids whose aliases were read into the alias table (once per
-- id), so the names pass asks only for artists it hasn't read yet.
CREATE TABLE alias_import (
    provider    TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    fetched_at  TEXT NOT NULL,
    PRIMARY KEY (provider, provider_id)
);

-- +goose Down
DROP TABLE alias_import;
