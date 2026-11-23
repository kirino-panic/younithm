-- +goose Up
-- Names keep their spelling as written. raw_string / name stay the
-- normalised key used for matching and search; display is how a person, a
-- listen or the catalogue wrote it. NULL means not known yet.
ALTER TABLE alias ADD COLUMN display TEXT;
ALTER TABLE search_name ADD COLUMN display TEXT;

-- Fill what listens show: the most common spelling of each key. SQLite's
-- lower() only folds ASCII; keys it can't rebuild stay NULL and are filled
-- the next time a listen or the catalogue writes them.
CREATE TEMP TABLE spelling AS
SELECT 'artist' AS kind, lower(trim(raw_artist)) AS k, trim(raw_artist) AS name, COUNT(*) AS n
  FROM scrobble WHERE raw_artist IS NOT NULL GROUP BY raw_artist
UNION ALL
SELECT 'recording', lower(trim(raw_artist)) || char(31) || lower(trim(raw_title)), trim(raw_title), COUNT(*)
  FROM scrobble WHERE raw_artist IS NOT NULL AND raw_title IS NOT NULL GROUP BY raw_artist, raw_title
UNION ALL
SELECT 'release', lower(trim(raw_album)), trim(raw_album), COUNT(*)
  FROM scrobble WHERE raw_album IS NOT NULL GROUP BY raw_album
-- Then the library's own names, then names read from the catalogues.
UNION ALL SELECT 'artist', lower(trim(name)), trim(name), 0 FROM artist
UNION ALL SELECT 'release', lower(trim(title)), trim(title), 0 FROM release
UNION ALL SELECT 'recording', lower(trim(a.name)) || char(31) || lower(trim(r.title)), trim(r.title), 0
  FROM recording r JOIN recording_artist ra ON ra.recording_id = r.id JOIN artist a ON a.id = ra.artist_id
UNION ALL SELECT entity_type, lower(trim(name)), trim(name), -1 FROM entity_name WHERE entity_type <> 'recording';
CREATE INDEX temp.spelling_k ON spelling (kind, k);
UPDATE alias SET display = (SELECT name FROM spelling s WHERE s.kind = alias.kind AND s.k = alias.raw_string ORDER BY n DESC, name LIMIT 1)
 WHERE display IS NULL;
UPDATE search_name SET display = (SELECT name FROM spelling s WHERE s.kind = search_name.kind AND s.k = search_name.name ORDER BY n DESC, name LIMIT 1)
 WHERE display IS NULL;
DROP TABLE spelling;

-- A track's name whose artist part came from a catalogue credit: take the
-- title as this track's listens, own title or catalogue names write it.
CREATE TEMP TABLE title_spelling AS
SELECT recording_id AS id, lower(trim(raw_title)) AS k, trim(raw_title) AS name, COUNT(*) AS n
  FROM scrobble WHERE recording_id IS NOT NULL AND raw_title IS NOT NULL GROUP BY recording_id, raw_title
UNION ALL SELECT id, lower(trim(title)), trim(title), 0 FROM recording
UNION ALL SELECT entity_id, lower(trim(name)), trim(name), -1 FROM entity_name WHERE entity_type = 'recording';
CREATE INDEX temp.title_spelling_k ON title_spelling (id, k);
UPDATE alias SET display = (SELECT name FROM title_spelling s
    WHERE s.id = alias.entity_id AND s.k = substr(alias.raw_string, instr(alias.raw_string, char(31)) + 1)
    ORDER BY n DESC, name LIMIT 1)
 WHERE kind = 'recording' AND display IS NULL;
DROP TABLE title_spelling;

-- Read MusicBrainz aliases again so their spelling is filled in.
DELETE FROM alias_import;

-- +goose Down
ALTER TABLE search_name DROP COLUMN display;
ALTER TABLE alias DROP COLUMN display;
