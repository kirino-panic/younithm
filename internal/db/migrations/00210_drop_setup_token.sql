-- +goose Up

-- First run no longer needs a token from the log: the first visitor creates
-- the admin account.
DROP TABLE IF EXISTS setup_token;

-- +goose Down
CREATE TABLE setup_token (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    token_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);
