-- +goose Up

-- Provider credentials (Spotify client secret, Apple .p8 key, ...), encrypted
-- with the master key (internal/secrets). Write-only through the HTTP API:
-- only metadata (set? last-4 hint) is ever returned.
CREATE TABLE provider_secret (
    name       TEXT PRIMARY KEY,   -- e.g. 'spotify.client_secret'
    ciphertext TEXT NOT NULL,      -- 'ys1:...' versioned AES-256-GCM
    hint       TEXT NOT NULL DEFAULT '', -- last 4 chars, '' for short values
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- +goose Down

DROP TABLE provider_secret;
