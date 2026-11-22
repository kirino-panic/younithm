-- +goose Up
-- A track marked by hand as on no album: it is taken off every album, and
-- nothing (a listen, an import, a catalogue) puts it on one again. Adding it to
-- an album by hand clears the mark.
ALTER TABLE recording ADD COLUMN no_album INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE recording DROP COLUMN no_album;
