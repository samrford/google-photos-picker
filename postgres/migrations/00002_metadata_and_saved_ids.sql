-- +goose Up
-- saved_ids: This is really free-form IDs returned by the sink (URLs, storage keys, UUIDs)
-- so renaming here for clarity. metadata: opaque caller
-- context attached at StartImport and threaded back to the sink.
ALTER TABLE photopicker_imports RENAME COLUMN image_urls TO saved_ids;
ALTER TABLE photopicker_imports ADD COLUMN metadata JSONB NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE photopicker_imports DROP COLUMN metadata;
ALTER TABLE photopicker_imports RENAME COLUMN saved_ids TO image_urls;
