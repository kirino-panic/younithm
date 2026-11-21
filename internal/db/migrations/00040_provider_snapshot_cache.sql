-- +goose Up

-- Extend provider_snapshot (created in 00001) into a proper cache:
--   status      'ok' = raw_json holds the provider response;
--               'not_found' = negative cache entry (raw_json is '{}')
--   expires_at  when the entry should be refetched; NULL = never expires
--   fetch_count how often we have (re)fetched it, for debugging/backoff
ALTER TABLE provider_snapshot ADD COLUMN status TEXT NOT NULL DEFAULT 'ok'
    CHECK (status IN ('ok', 'not_found'));
ALTER TABLE provider_snapshot ADD COLUMN expires_at TEXT;
ALTER TABLE provider_snapshot ADD COLUMN fetch_count INTEGER NOT NULL DEFAULT 1;

CREATE INDEX provider_snapshot_expiry ON provider_snapshot(expires_at);

-- +goose Down

DROP INDEX provider_snapshot_expiry;
ALTER TABLE provider_snapshot DROP COLUMN fetch_count;
ALTER TABLE provider_snapshot DROP COLUMN expires_at;
ALTER TABLE provider_snapshot DROP COLUMN status;
