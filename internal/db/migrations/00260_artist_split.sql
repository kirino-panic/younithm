-- +goose Up
-- A split artist: a credit string that names several artists ("Danny Brown &
-- Underscores") was split by hand into those artists. Later listens credited
-- with that string (normalised, as alias keys are) get these artists, in
-- position order, instead of an artist named the whole string. A rule missing
-- a position (one of its artists deleted) is ignored.
CREATE TABLE artist_split (
    raw_string  TEXT NOT NULL,
    position    INTEGER NOT NULL,
    artist_id   INTEGER NOT NULL REFERENCES artist(id) ON DELETE CASCADE,
    join_phrase TEXT,
    PRIMARY KEY (raw_string, position)
);
CREATE INDEX artist_split_artist ON artist_split(artist_id);

-- +goose Down
DROP TABLE artist_split;
