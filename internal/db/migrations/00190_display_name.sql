-- +goose Up

-- display_name is a name a person set by hand to show for the entity. It
-- shows over catalogue names and the stored name, which it leaves untouched;
-- NULL hands the name back to the naming order. Renames made before this
-- column existed changed the stored name, so it is carried over as the
-- display name to keep showing.
ALTER TABLE artist ADD COLUMN display_name TEXT;
ALTER TABLE recording ADD COLUMN display_name TEXT;
ALTER TABLE release ADD COLUMN display_name TEXT;

UPDATE artist SET display_name = name WHERE id IN (SELECT entity_id FROM edit_log
  WHERE entity_type = 'artist' AND scope = 'entity' AND undone_at IS NULL AND instr(after_json, '"name"') > 0);
UPDATE recording SET display_name = title WHERE id IN (SELECT entity_id FROM edit_log
  WHERE entity_type = 'recording' AND scope = 'entity' AND undone_at IS NULL AND instr(after_json, '"title"') > 0);
UPDATE release SET display_name = title WHERE id IN (SELECT entity_id FROM edit_log
  WHERE entity_type = 'release' AND scope = 'entity' AND undone_at IS NULL AND instr(after_json, '"title"') > 0);

-- +goose Down
ALTER TABLE artist DROP COLUMN display_name;
ALTER TABLE recording DROP COLUMN display_name;
ALTER TABLE release DROP COLUMN display_name;
