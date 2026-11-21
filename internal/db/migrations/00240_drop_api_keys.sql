-- +goose Up
-- API keys are gone: scrobblers use the one token chosen in Connections.
DROP TABLE IF EXISTS api_key;

-- +goose Down
CREATE TABLE api_key (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL DEFAULT 1,
    name         TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_used_at TEXT,
    last_used_ip TEXT,
    revoked_at   TEXT
);
CREATE INDEX api_key_user ON api_key(user_id, revoked_at);
