-- +goose Up

-- Imports that need more than one file (Apple Music: Play Activity + Daily
-- Tracks, docs/imports.md). A job is created with the first
-- file and waits ('awaiting_files' = 1, status stays 'queued' so the status
-- CHECK needs no rebuild) until every required file is present. The worker never
-- claims it, so nothing is parsed, previewed, joined or applied on half the data.
ALTER TABLE import_job ADD COLUMN awaiting_files INTEGER NOT NULL DEFAULT 0;

-- One row per file (or per entry of an uploaded zip) of a multi-file job. Single
-- file imports keep their file in import_job itself and have no rows here.
CREATE TABLE import_job_file (
    id          INTEGER PRIMARY KEY,
    job_id      INTEGER NOT NULL REFERENCES import_job(id) ON DELETE CASCADE,
    role        TEXT NOT NULL,              -- e.g. 'play_activity', 'daily_tracks'
    filename    TEXT NOT NULL,
    entry       TEXT NOT NULL DEFAULT '',   -- path inside the zip; '' for a plain file
    file_path   TEXT,                       -- NULL once the upload is deleted
    file_size   INTEGER NOT NULL DEFAULT 0, -- of the uploaded file (the zip, for entries)
    file_sha256 TEXT NOT NULL,
    added_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX import_job_file_job ON import_job_file(job_id, role);

-- +goose Down

DROP INDEX import_job_file_job;
DROP TABLE import_job_file;
ALTER TABLE import_job DROP COLUMN awaiting_files;
