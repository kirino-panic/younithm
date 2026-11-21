-- +goose Up
-- The name an artist is credited as on a track or album when it differs from
-- the artist's own name (MusicBrainz "artist as credited": T-SQUARE credited
-- as "The Square"). NULL means the artist's name. Credit lines show it; the
-- artist's own page and lists keep the artist's name.
ALTER TABLE recording_artist ADD COLUMN credited_name TEXT;
ALTER TABLE release_artist   ADD COLUMN credited_name TEXT;

-- +goose Down
ALTER TABLE release_artist   DROP COLUMN credited_name;
ALTER TABLE recording_artist DROP COLUMN credited_name;
