-- +goose Up

-- Names by language, one row per catalogue (internal/enrich/languages.go):
-- MusicBrainz aliases, Apple Music storefronts and Spotify can each word an
-- entity in a language, and the display picks among them. provider_id is the
-- catalogue id the name was read for, so a changed link asks again. An empty
-- name records that the catalogue was asked and had none. authoritative is 1
-- when the catalogue states the language (a MusicBrainz alias with a locale,
-- an Apple storefront that supports it), 0 when it may have fallen back to
-- another wording (Spotify). The rows the per-track lookup wrote are dropped:
-- the batched lookups fill them again.
DROP TABLE entity_name;
CREATE TABLE entity_name (
    entity_type   TEXT NOT NULL CHECK (entity_type IN ('artist', 'recording', 'release')),
    entity_id     INTEGER NOT NULL,
    lang          TEXT NOT NULL,
    provider      TEXT NOT NULL,
    provider_id   TEXT NOT NULL,
    name          TEXT NOT NULL,
    primary_name  INTEGER NOT NULL DEFAULT 0,
    authoritative INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (entity_type, entity_id, lang, provider)
);

-- +goose Down

DROP TABLE entity_name;
CREATE TABLE entity_name (
    entity_type TEXT NOT NULL CHECK (entity_type IN ('artist', 'recording', 'release')),
    entity_id   INTEGER NOT NULL,
    lang        TEXT NOT NULL,
    name        TEXT NOT NULL,
    provider    TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (entity_type, entity_id, lang)
);
