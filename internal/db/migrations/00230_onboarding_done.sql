-- +goose Up

-- Installs that already have an admin finished setup before the welcome flow
-- existed: don't send them through it.
INSERT INTO admin_setting (key, value)
SELECT 'onboarding.done', '1' WHERE EXISTS (SELECT 1 FROM admin_user)
ON CONFLICT(key) DO NOTHING;

-- +goose Down
DELETE FROM admin_setting WHERE key = 'onboarding.done';
