-- +goose Up

-- Single admin for v1; user_id kept everywhere so multi-user can come later.
CREATE TABLE admin_user (
    id                 INTEGER PRIMARY KEY,
    username           TEXT NOT NULL UNIQUE,
    password_hash      TEXT NOT NULL, -- argon2id PHC string
    totp_secret        TEXT,          -- base32; see totp_secret_format
    totp_secret_format INTEGER NOT NULL DEFAULT 0, -- 0 = plaintext, 1 = encrypted with master key (later track)
    totp_enabled       INTEGER NOT NULL DEFAULT 0,
    totp_last_step     INTEGER NOT NULL DEFAULT 0, -- replay protection
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE auth_session (
    id            INTEGER PRIMARY KEY,
    token_hash    TEXT NOT NULL UNIQUE, -- sha256 hex of the cookie token
    user_id       INTEGER NOT NULL REFERENCES admin_user(id) ON DELETE CASCADE,
    csrf_token    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    expires_at    TEXT NOT NULL, -- absolute lifetime
    step_up_until TEXT,
    ip            TEXT,
    user_agent    TEXT
);
CREATE INDEX auth_session_user ON auth_session(user_id);

CREATE TABLE totp_recovery_code (
    id        INTEGER PRIMARY KEY,
    user_id   INTEGER NOT NULL REFERENCES admin_user(id) ON DELETE CASCADE,
    code_hash TEXT NOT NULL, -- sha256 hex (codes are high-entropy random)
    used_at   TEXT
);
CREATE INDEX totp_recovery_user ON totp_recovery_code(user_id);

-- Failed-attempt counters keyed by "ip:<addr>" or "user:<name>" or "setup:<addr>".
CREATE TABLE auth_throttle (
    key          TEXT PRIMARY KEY,
    fails        INTEGER NOT NULL DEFAULT 0,
    locked_until TEXT,
    updated_at   TEXT NOT NULL
);

-- One-time first-run setup token (hash only). Regenerated on each start until used.
CREATE TABLE setup_token (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    token_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);

-- Reserved for the API-keys track: scoped, hashed at rest, shown once.
-- Auth exposes the KeyAuthenticator hook; this table just reserves the shape.
CREATE TABLE api_key (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL DEFAULT 1,
    name         TEXT NOT NULL,
    prefix       TEXT NOT NULL,        -- first chars, displayable
    key_hash     TEXT NOT NULL UNIQUE, -- sha256 hex
    scopes       TEXT NOT NULL,        -- space-separated, e.g. "scrobble:write"
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_used_at TEXT,
    revoked_at   TEXT
);

-- +goose Down

DROP TABLE api_key;
DROP TABLE setup_token;
DROP TABLE auth_throttle;
DROP TABLE totp_recovery_code;
DROP TABLE auth_session;
DROP TABLE admin_user;
