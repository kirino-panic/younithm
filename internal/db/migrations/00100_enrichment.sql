-- +goose Up

-- Enrichment queue: one row per recording that should be matched against the
-- metadata providers. The review queue is not a table of its own: it is the
-- set of external_link rows with status = 'candidate'.
CREATE TABLE enrichment_job (
    id              INTEGER PRIMARY KEY,
    recording_id    INTEGER NOT NULL UNIQUE REFERENCES recording(id) ON DELETE CASCADE,
    status          TEXT NOT NULL CHECK (status IN ('queued', 'running', 'done', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    outcome         TEXT CHECK (outcome IN ('linked', 'candidates', 'no_match')),
    last_error      TEXT,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX enrichment_job_due ON enrichment_job(status, next_attempt_at);

-- Candidates the admin rejected; the matcher must not propose them again.
CREATE TABLE link_rejection (
    entity_type TEXT NOT NULL CHECK (entity_type IN ('artist', 'recording', 'release')),
    entity_id   INTEGER NOT NULL,
    provider    TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (entity_type, entity_id, provider, provider_id)
);

-- +goose Down

DROP TABLE link_rejection;
DROP INDEX enrichment_job_due;
DROP TABLE enrichment_job;
