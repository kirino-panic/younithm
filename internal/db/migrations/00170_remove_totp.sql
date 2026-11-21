-- +goose Up

-- Two-factor authentication was removed: no TOTP seed, replay step or recovery
-- codes are kept any more.
DROP TABLE totp_recovery_code;
ALTER TABLE admin_user DROP COLUMN totp_secret;
ALTER TABLE admin_user DROP COLUMN totp_secret_format;
ALTER TABLE admin_user DROP COLUMN totp_enabled;
ALTER TABLE admin_user DROP COLUMN totp_last_step;

-- +goose Down

ALTER TABLE admin_user ADD COLUMN totp_secret TEXT;
ALTER TABLE admin_user ADD COLUMN totp_secret_format INTEGER NOT NULL DEFAULT 0;
ALTER TABLE admin_user ADD COLUMN totp_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE admin_user ADD COLUMN totp_last_step INTEGER NOT NULL DEFAULT 0;
CREATE TABLE totp_recovery_code (
    id        INTEGER PRIMARY KEY,
    user_id   INTEGER NOT NULL REFERENCES admin_user(id) ON DELETE CASCADE,
    code_hash TEXT NOT NULL,
    used_at   TEXT
);
CREATE INDEX totp_recovery_user ON totp_recovery_code(user_id);
