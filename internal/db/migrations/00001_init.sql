-- +goose Up

CREATE TABLE artist (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    sort_name  TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE recording (
    id          INTEGER PRIMARY KEY,
    title       TEXT NOT NULL,
    duration_ms INTEGER,
    work_id     INTEGER, -- reserved for grouping versions of a song (post-v1)
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE release (
    id           INTEGER PRIMARY KEY,
    title        TEXT NOT NULL,
    release_date TEXT,
    release_type TEXT,
    artwork_path TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE recording_artist (
    recording_id INTEGER NOT NULL REFERENCES recording(id) ON DELETE CASCADE,
    artist_id    INTEGER NOT NULL REFERENCES artist(id) ON DELETE CASCADE,
    role         TEXT NOT NULL DEFAULT 'primary',
    position     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (recording_id, artist_id, role)
);

CREATE TABLE release_artist (
    release_id INTEGER NOT NULL REFERENCES release(id) ON DELETE CASCADE,
    artist_id  INTEGER NOT NULL REFERENCES artist(id) ON DELETE CASCADE,
    position   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (release_id, artist_id)
);

-- "This recording belongs to these releases."
CREATE TABLE recording_release (
    recording_id INTEGER NOT NULL REFERENCES recording(id) ON DELETE CASCADE,
    release_id   INTEGER NOT NULL REFERENCES release(id) ON DELETE CASCADE,
    disc         INTEGER NOT NULL DEFAULT 1,
    position     INTEGER,
    is_primary   INTEGER NOT NULL DEFAULT 0, -- drives default artwork/album
    PRIMARY KEY (recording_id, release_id)
);

-- Scrobbles are events; raw_* values are exactly as submitted and never rewritten.
CREATE TABLE scrobble (
    id                 INTEGER PRIMARY KEY,
    user_id            INTEGER NOT NULL DEFAULT 1,
    played_at          TEXT NOT NULL,
    duration_played_ms INTEGER,
    source             TEXT,
    recording_id       INTEGER REFERENCES recording(id) ON DELETE SET NULL,
    release_id         INTEGER REFERENCES release(id) ON DELETE SET NULL,
    raw_artist         TEXT NOT NULL,
    raw_title          TEXT NOT NULL,
    raw_album          TEXT,
    raw_payload        TEXT,
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX scrobble_played_at   ON scrobble(user_id, played_at);
CREATE INDEX scrobble_recording   ON scrobble(recording_id);
CREATE INDEX scrobble_release     ON scrobble(release_id);

-- Links internal entities to provider IDs (MusicBrainz, Spotify, Apple, ISRC...).
-- One entity may have many IDs per provider.
CREATE TABLE external_link (
    id           INTEGER PRIMARY KEY,
    entity_type  TEXT NOT NULL CHECK (entity_type IN ('artist', 'recording', 'release')),
    entity_id    INTEGER NOT NULL,
    provider     TEXT NOT NULL,
    provider_id  TEXT NOT NULL,
    match_method TEXT NOT NULL CHECK (match_method IN ('submitted_mbid', 'isrc', 'fuzzy', 'manual')),
    confidence   REAL,
    status       TEXT NOT NULL DEFAULT 'auto' CHECK (status IN ('confirmed', 'auto', 'candidate')),
    pinned       INTEGER NOT NULL DEFAULT 0, -- manual links are never overwritten
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (entity_type, entity_id, provider, provider_id)
);
CREATE INDEX external_link_lookup ON external_link(entity_type, provider, provider_id);

-- Raw provider responses, so canonical fields can be re-derived without refetching.
CREATE TABLE provider_snapshot (
    provider    TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    kind        TEXT NOT NULL,
    fetched_at  TEXT NOT NULL,
    raw_json    TEXT NOT NULL,
    PRIMARY KEY (provider, provider_id, kind)
);

-- Learned from edits/merges; applied before matching.
CREATE TABLE alias (
    id         INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('artist', 'recording', 'release')),
    raw_string TEXT NOT NULL,
    entity_id  INTEGER NOT NULL,
    UNIQUE (kind, raw_string)
);

CREATE TABLE edit_log (
    id          INTEGER PRIMARY KEY,
    at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    actor       TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id   INTEGER NOT NULL,
    scope       TEXT NOT NULL CHECK (scope IN ('scrobble', 'entity', 'merge', 'bulk')),
    before_json TEXT,
    after_json  TEXT
);

-- +goose Down

DROP TABLE edit_log;
DROP TABLE alias;
DROP TABLE provider_snapshot;
DROP TABLE external_link;
DROP TABLE scrobble;
DROP TABLE recording_release;
DROP TABLE release_artist;
DROP TABLE recording_artist;
DROP TABLE release;
DROP TABLE recording;
DROP TABLE artist;
