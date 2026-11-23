-- +goose Up
-- Search-only names for every kind (artist_search_name was artists only). A
-- name here finds the entity in search; a listen is never matched by it. Rows
-- read from a catalogue keep the id they came from (provider, provider_id) and
-- go when the entity no longer holds it; a row a person made has provider ''.
CREATE TABLE search_name (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('artist', 'recording', 'release')),
    entity_id   INTEGER NOT NULL,
    name        TEXT NOT NULL, -- normalised like alias keys
    provider    TEXT NOT NULL DEFAULT '',
    provider_id TEXT NOT NULL DEFAULT '',
    UNIQUE (kind, entity_id, name)
);
CREATE INDEX search_name_by_source ON search_name (provider, provider_id);
INSERT OR IGNORE INTO search_name (kind, entity_id, name, provider, provider_id)
  SELECT 'artist', artist_id, name, provider, provider_id FROM artist_search_name;
DROP TABLE artist_search_name;

-- Names a person removed from an entity: no catalogue or listen adds them back.
CREATE TABLE alias_rejection (
    kind       TEXT NOT NULL,
    entity_id  INTEGER NOT NULL,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (kind, entity_id, name)
);

-- edit_log: an 'alias' scope. Like 'link', it does not make a recording count
-- as edited by hand. SQLite cannot alter a CHECK, so the table is rebuilt.
CREATE TABLE edit_log_new (
    id          INTEGER PRIMARY KEY,
    at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    actor       TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id   INTEGER NOT NULL,
    scope       TEXT NOT NULL CHECK (scope IN ('scrobble', 'entity', 'merge', 'bulk', 'album', 'undo', 'credits', 'link', 'alias')),
    before_json TEXT,
    after_json  TEXT,
    summary     TEXT NOT NULL DEFAULT '',
    undo_of     INTEGER REFERENCES edit_log_new(id),
    undone_at   TEXT,
    undone_by   INTEGER
);
INSERT INTO edit_log_new SELECT id, at, actor, entity_type, entity_id, scope, before_json, after_json, summary, undo_of, undone_at, undone_by FROM edit_log;
DROP TABLE edit_log;
ALTER TABLE edit_log_new RENAME TO edit_log;
CREATE INDEX edit_log_entity ON edit_log(entity_type, entity_id);
CREATE INDEX edit_log_at ON edit_log(at);

-- +goose Down
DELETE FROM edit_log WHERE scope = 'alias';
CREATE TABLE edit_log_old (
    id          INTEGER PRIMARY KEY,
    at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    actor       TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id   INTEGER NOT NULL,
    scope       TEXT NOT NULL CHECK (scope IN ('scrobble', 'entity', 'merge', 'bulk', 'album', 'undo', 'credits', 'link')),
    before_json TEXT,
    after_json  TEXT,
    summary     TEXT NOT NULL DEFAULT '',
    undo_of     INTEGER REFERENCES edit_log_old(id),
    undone_at   TEXT,
    undone_by   INTEGER
);
INSERT INTO edit_log_old SELECT * FROM edit_log;
DROP TABLE edit_log;
ALTER TABLE edit_log_old RENAME TO edit_log;
CREATE INDEX edit_log_entity ON edit_log(entity_type, entity_id);
CREATE INDEX edit_log_at ON edit_log(at);
DROP TABLE alias_rejection;
CREATE TABLE artist_search_name (
    artist_id   INTEGER NOT NULL REFERENCES artist(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    provider    TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    PRIMARY KEY (artist_id, name, provider, provider_id)
);
CREATE INDEX artist_search_name_by_source ON artist_search_name (provider, provider_id);
INSERT INTO artist_search_name SELECT entity_id, name, provider, provider_id FROM search_name WHERE kind='artist';
DROP TABLE search_name;
