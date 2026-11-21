-- +goose Up

-- auto_confirm: the admin has already said yes to this import while its file
-- was still being read (onboarding confirms everything at the end). When the
-- preview finishes, the apply phase is queued at once instead of waiting.
ALTER TABLE import_job ADD COLUMN auto_confirm INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE import_job DROP COLUMN auto_confirm;
