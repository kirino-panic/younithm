-- +goose Up

-- One row per import. Lifecycle (see docs/imports.md):
--   queued -> running (phase=preview) -> awaiting_confirmation
--          -> queued -> running (phase=apply) -> done
-- plus failed / cancelled from any non-terminal state.
CREATE TABLE import_job (
    id               INTEGER PRIMARY KEY,
    user_id          INTEGER NOT NULL DEFAULT 1,
    status           TEXT NOT NULL CHECK (status IN
                       ('queued', 'running', 'awaiting_confirmation', 'done', 'failed', 'cancelled')),
    phase            TEXT NOT NULL DEFAULT 'preview' CHECK (phase IN ('preview', 'apply')),
    filename         TEXT NOT NULL,
    format           TEXT NOT NULL,           -- parser name actually used
    format_detected  INTEGER NOT NULL DEFAULT 0, -- 1 if auto-detected
    file_path        TEXT,                    -- NULL once the upload is deleted
    file_size        INTEGER NOT NULL DEFAULT 0,
    file_sha256      TEXT NOT NULL,
    options_json     TEXT NOT NULL DEFAULT '{}', -- dedupe window, spotify min duration
    -- progress for the current phase
    rows_seen        INTEGER NOT NULL DEFAULT 0,
    parsed_count     INTEGER NOT NULL DEFAULT 0,
    skipped_count    INTEGER NOT NULL DEFAULT 0,
    error_count      INTEGER NOT NULL DEFAULT 0,
    new_count        INTEGER NOT NULL DEFAULT 0,
    duplicate_count  INTEGER NOT NULL DEFAULT 0,
    unmatched_count  INTEGER NOT NULL DEFAULT 0, -- new listens with no recording link
    inserted_count   INTEGER NOT NULL DEFAULT 0, -- apply phase only
    preview_json     TEXT,                    -- frozen summary of the dry run
    result_json      TEXT,                    -- summary of the apply
    error            TEXT,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    started_at       TEXT,
    finished_at      TEXT
);
CREATE INDEX import_job_status ON import_job(status, id);

-- Which import created a scrobble (NULL = live ingest). Lets a retried apply
-- recognise its own earlier rows and lets an import be traced or undone.
ALTER TABLE scrobble ADD COLUMN import_job_id INTEGER REFERENCES import_job(id) ON DELETE SET NULL;
CREATE INDEX scrobble_import_job ON scrobble(import_job_id);

-- +goose Down

DROP INDEX scrobble_import_job;
ALTER TABLE scrobble DROP COLUMN import_job_id;
DROP TABLE import_job;
