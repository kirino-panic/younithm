-- +goose Up

-- The api_key table itself was created in 00020_auth.sql. This adds what the
-- API-keys track needs on top: a lookup index by owner and the last client IP
-- (diagnostics only, never used for access decisions).
ALTER TABLE api_key ADD COLUMN last_used_ip TEXT;
CREATE INDEX api_key_user ON api_key(user_id, revoked_at);

-- +goose Down

DROP INDEX api_key_user;
ALTER TABLE api_key DROP COLUMN last_used_ip;
