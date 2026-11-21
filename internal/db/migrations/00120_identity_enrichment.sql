-- +goose Up

-- Identity enrichment (docs/identity.md).

-- external_link.match_method grows: 'lookup' = learned by asking a provider
-- about an id we already hold (catalog id -> ISRC, credited artists, album).
-- SQLite cannot alter a CHECK, so the table is rebuilt as in 00110.
CREATE TABLE external_link_new (
    id           INTEGER PRIMARY KEY,
    entity_type  TEXT NOT NULL CHECK (entity_type IN ('artist', 'recording', 'release')),
    entity_id    INTEGER NOT NULL,
    provider     TEXT NOT NULL,
    provider_id  TEXT NOT NULL,
    match_method TEXT NOT NULL CHECK (match_method IN
        ('submitted_mbid', 'isrc', 'fuzzy', 'manual', 'submitted_id', 'url_rel', 'mapping', 'name', 'lookup')),
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

-- Enrichment order: most-played recordings first. Set when the job is queued.
ALTER TABLE enrichment_job ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
DROP INDEX enrichment_job_due;
CREATE INDEX enrichment_job_due ON enrichment_job(status, priority DESC, next_attempt_at);

-- Whether fuzzy matching may auto-link this recording. Imports of sources with
-- no ids ask the user; unchecked = 0, and fuzzy hits become candidates only.
-- Any later listen or import that consents sets it back to 1.
ALTER TABLE recording ADD COLUMN fuzzy_consent INTEGER NOT NULL DEFAULT 1;

-- Name lookups the resolver runs for every listen (lower(trim(x)) = ?): without
-- these an import scans the table once per row.
CREATE INDEX artist_name_norm    ON artist(lower(trim(name)));
CREATE INDEX recording_title_norm ON recording(lower(trim(title)));
CREATE INDEX release_title_norm  ON release(lower(trim(title)));

-- +goose Down

DROP INDEX release_title_norm;
DROP INDEX recording_title_norm;
DROP INDEX artist_name_norm;
ALTER TABLE recording DROP COLUMN fuzzy_consent;
DROP INDEX enrichment_job_due;
CREATE INDEX enrichment_job_due ON enrichment_job(status, next_attempt_at);
ALTER TABLE enrichment_job DROP COLUMN priority;

CREATE TABLE external_link_old (
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
INSERT INTO external_link_old (id, entity_type, entity_id, provider, provider_id, match_method, confidence, status, pinned, created_at)
    SELECT id, entity_type, entity_id, provider, provider_id,
           CASE WHEN match_method = 'lookup' THEN 'submitted_id' ELSE match_method END,
           confidence, status, pinned, created_at FROM external_link;
DROP TABLE external_link;
ALTER TABLE external_link_old RENAME TO external_link;
CREATE INDEX external_link_lookup ON external_link(entity_type, provider, provider_id);
