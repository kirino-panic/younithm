-- +goose Up

-- Identity graph (docs/identity.md).

-- An artist is "verified" once at least one provider ID corroborates it;
-- "unsplit" marks a collaboration string ("A feat. B") that has not been
-- split into its credited artists yet. Existing artists with a provider link
-- are verified, the rest are not.
ALTER TABLE artist ADD COLUMN verified INTEGER NOT NULL DEFAULT 1;
ALTER TABLE artist ADD COLUMN unsplit  INTEGER NOT NULL DEFAULT 0;
UPDATE artist SET verified = EXISTS (
    SELECT 1 FROM external_link l WHERE l.entity_type = 'artist' AND l.entity_id = artist.id
);

-- Text that follows a credited artist when the credits are displayed
-- ("Ninajirachi" + " & " + "Porter Robinson").
ALTER TABLE recording_artist ADD COLUMN join_phrase TEXT;
ALTER TABLE release_artist   ADD COLUMN join_phrase TEXT;

-- Grouping attribute for "Associated releases"; never part of identity.
ALTER TABLE release ADD COLUMN release_group_mbid TEXT;
CREATE INDEX release_group_mbid ON release(release_group_mbid) WHERE release_group_mbid IS NOT NULL;

-- external_link.match_method grows (SQLite cannot alter a CHECK): client-submitted
-- non-MBID ids, MusicBrainz-curated url_rels, ListenBrainz mbid_mapping, name.
CREATE TABLE external_link_new (
    id           INTEGER PRIMARY KEY,
    entity_type  TEXT NOT NULL CHECK (entity_type IN ('artist', 'recording', 'release')),
    entity_id    INTEGER NOT NULL,
    provider     TEXT NOT NULL,
    provider_id  TEXT NOT NULL,
    match_method TEXT NOT NULL CHECK (match_method IN
        ('submitted_mbid', 'isrc', 'fuzzy', 'manual', 'submitted_id', 'url_rel', 'mapping', 'name')),
    confidence   REAL,
    status       TEXT NOT NULL DEFAULT 'auto' CHECK (status IN ('confirmed', 'auto', 'candidate')),
    pinned       INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (entity_type, entity_id, provider, provider_id)
);
INSERT INTO external_link_new (id, entity_type, entity_id, provider, provider_id, match_method, confidence, status, pinned, created_at)
    SELECT id, entity_type, entity_id, provider, provider_id, match_method, confidence, status, pinned, created_at FROM external_link;
DROP TABLE external_link;
ALTER TABLE external_link_new RENAME TO external_link;
CREATE INDEX external_link_lookup ON external_link(entity_type, provider, provider_id);

-- Pairs of entities the resolver would not merge on its own (same name,
-- conflicting ids, ...). The review queue lists them for a manual merge.
CREATE TABLE merge_candidate (
    id         INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('artist', 'recording', 'release')),
    entity_id  INTEGER NOT NULL,
    other_id   INTEGER NOT NULL,
    reason     TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (kind, entity_id, other_id)
);

-- Backfill: re-resolve existing scrobbles from raw_payload. last_scrobble_id
-- is the resume cursor; counters is a JSON report.
CREATE TABLE identity_backfill (
    id               INTEGER PRIMARY KEY,
    status           TEXT NOT NULL CHECK (status IN ('running', 'done', 'failed', 'cancelled')),
    dry_run          INTEGER NOT NULL DEFAULT 0,
    last_scrobble_id INTEGER NOT NULL DEFAULT 0,
    total            INTEGER NOT NULL DEFAULT 0,
    counters         TEXT NOT NULL DEFAULT '{}',
    error            TEXT,
    started_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    finished_at      TEXT
);

-- +goose Down

DROP TABLE identity_backfill;
DROP TABLE merge_candidate;

CREATE TABLE external_link_old (
    id           INTEGER PRIMARY KEY,
    entity_type  TEXT NOT NULL CHECK (entity_type IN ('artist', 'recording', 'release')),
    entity_id    INTEGER NOT NULL,
    provider     TEXT NOT NULL,
    provider_id  TEXT NOT NULL,
    match_method TEXT NOT NULL CHECK (match_method IN ('submitted_mbid', 'isrc', 'fuzzy', 'manual')),
    confidence   REAL,
    status       TEXT NOT NULL DEFAULT 'auto' CHECK (status IN ('confirmed', 'auto', 'candidate')),
    pinned       INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (entity_type, entity_id, provider, provider_id)
);
INSERT INTO external_link_old (id, entity_type, entity_id, provider, provider_id, match_method, confidence, status, pinned, created_at)
    SELECT id, entity_type, entity_id, provider, provider_id,
           CASE WHEN match_method IN ('submitted_mbid', 'isrc', 'fuzzy', 'manual') THEN match_method ELSE 'fuzzy' END,
           confidence, status, pinned, created_at FROM external_link;
DROP TABLE external_link;
ALTER TABLE external_link_old RENAME TO external_link;
CREATE INDEX external_link_lookup ON external_link(entity_type, provider, provider_id);

DROP INDEX release_group_mbid;
ALTER TABLE release DROP COLUMN release_group_mbid;
ALTER TABLE release_artist DROP COLUMN join_phrase;
ALTER TABLE recording_artist DROP COLUMN join_phrase;
ALTER TABLE artist DROP COLUMN unsplit;
ALTER TABLE artist DROP COLUMN verified;
