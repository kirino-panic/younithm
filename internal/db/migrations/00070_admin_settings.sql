-- +goose Up

-- Small admin-owned key/value store for site settings: per-section public
-- visibility ('public.<section>' = '0'|'1'), the site default theme
-- ('theme.default') and admin custom CSS ('theme.custom_css'). Absent keys
-- mean the default (sections public, theme 'system', no custom CSS).
CREATE TABLE admin_setting (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- +goose Down

DROP TABLE admin_setting;
