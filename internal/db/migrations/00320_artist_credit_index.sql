-- +goose Up
-- An artist's page and its "Appears on" read credits by artist.
CREATE INDEX IF NOT EXISTS recording_artist_artist ON recording_artist (artist_id);
CREATE INDEX IF NOT EXISTS release_artist_artist ON release_artist (artist_id);

-- +goose Down
DROP INDEX IF EXISTS release_artist_artist;
DROP INDEX IF EXISTS recording_artist_artist;
