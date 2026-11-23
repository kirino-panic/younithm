-- +goose Up
-- Catalogue aliases are for finding an artist in search only: a listen is never
-- matched to an artist by them (MusicBrainz alias lists hold short or shared
-- names like "icy" or other projects' names). Each row keeps the id it came
-- from, so it goes when the artist no longer holds that id.
CREATE TABLE artist_search_name (
    artist_id   INTEGER NOT NULL REFERENCES artist(id) ON DELETE CASCADE,
    name        TEXT NOT NULL, -- normalised like alias keys
    provider    TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    PRIMARY KEY (artist_id, name, provider, provider_id)
);
CREATE INDEX artist_search_name_by_source ON artist_search_name (provider, provider_id);

-- Read again, into the new table, and remember which artist they were read for.
DROP TABLE alias_import;
CREATE TABLE alias_import (
    provider    TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    artist_id   INTEGER NOT NULL,
    fetched_at  TEXT NOT NULL,
    PRIMARY KEY (provider, provider_id)
);

-- +goose Down
DROP TABLE alias_import;
CREATE TABLE alias_import (
    provider    TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    fetched_at  TEXT NOT NULL,
    PRIMARY KEY (provider, provider_id)
);
DROP TABLE artist_search_name;
