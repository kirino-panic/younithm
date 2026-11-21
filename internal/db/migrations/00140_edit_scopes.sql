-- +goose Up

-- edit_log: two more scopes. 'credits' is a recording's or album's artist list
-- set by hand; 'link' is a catalogue link set or removed by hand (it does not
-- make a recording count as hand-edited, so lookups through the link still fill
-- its details). SQLite cannot alter a CHECK, so the table is rebuilt.
CREATE TABLE edit_log_new (
    id          INTEGER PRIMARY KEY,
    at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    actor       TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id   INTEGER NOT NULL,
    scope       TEXT NOT NULL CHECK (scope IN ('scrobble', 'entity', 'merge', 'bulk', 'album', 'undo', 'credits', 'link')),
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

DELETE FROM edit_log WHERE scope IN ('credits', 'link');
CREATE TABLE edit_log_old (
    id          INTEGER PRIMARY KEY,
    at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    actor       TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id   INTEGER NOT NULL,
    scope       TEXT NOT NULL CHECK (scope IN ('scrobble', 'entity', 'merge', 'bulk', 'album', 'undo')),
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
