-- +goose Up

-- Raw vs edited: raw_artist/raw_title/raw_album are never rewritten. The
-- played_at an edit replaces is preserved in orig_played_at the first time it
-- changes (NULL = never edited).
ALTER TABLE scrobble ADD COLUMN orig_played_at TEXT;
ALTER TABLE scrobble ADD COLUMN edited_at TEXT;

CREATE INDEX alias_entity ON alias(kind, entity_id);

-- edit_log: widen the scope check (SQLite cannot alter a CHECK), and add what
-- undo needs. before_json / after_json are aligned JSON arrays of row-level
-- changes: [{"t":table,"k":{pk},"r":{columns}|null}, ...]. r is null for the
-- missing side of an insert/delete; for updates it holds only changed columns.
CREATE TABLE edit_log_new (
    id          INTEGER PRIMARY KEY,
    at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    actor       TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id   INTEGER NOT NULL,
    scope       TEXT NOT NULL CHECK (scope IN ('scrobble', 'entity', 'merge', 'bulk', 'album', 'undo')),
    before_json TEXT,
    after_json  TEXT,
    summary     TEXT NOT NULL DEFAULT '',
    undo_of     INTEGER REFERENCES edit_log_new(id),
    undone_at   TEXT,
    undone_by   INTEGER
);
INSERT INTO edit_log_new (id, at, actor, entity_type, entity_id, scope, before_json, after_json)
    SELECT id, at, actor, entity_type, entity_id, scope, before_json, after_json FROM edit_log;
DROP TABLE edit_log;
ALTER TABLE edit_log_new RENAME TO edit_log;
CREATE INDEX edit_log_entity ON edit_log(entity_type, entity_id);
CREATE INDEX edit_log_at ON edit_log(at);

-- +goose Down

DROP INDEX edit_log_at;
DROP INDEX edit_log_entity;
DROP INDEX alias_entity;
ALTER TABLE scrobble DROP COLUMN edited_at;
ALTER TABLE scrobble DROP COLUMN orig_played_at;
