-- +goose Up

-- Names of an entity in other languages (internal/enrich/languages.go). The
-- stored name on artist, recording and release is never touched: this table
-- only adds wordings. One row per entity and language, and rows are written
-- once (a language already filled is not asked for again). A row whose name
-- equals the stored one is kept too: it records that the language was asked.
CREATE TABLE entity_name (
    entity_type TEXT NOT NULL CHECK (entity_type IN ('artist', 'recording', 'release')),
    entity_id   INTEGER NOT NULL,
    lang        TEXT NOT NULL,
    name        TEXT NOT NULL,
    provider    TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (entity_type, entity_id, lang)
);

-- +goose Down

DROP TABLE entity_name;
