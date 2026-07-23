-- +goose NO TRANSACTION

-- +goose Up
-- The series queue fingerprint aggregates active paths under each observed
-- root. Build its supporting index without blocking scanner writes to the
-- potentially large media_files table during deployment.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_media_files_folder_root_active
    ON media_files (media_folder_id, observed_root_path)
    WHERE missing_since IS NULL AND extra_id IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_media_files_folder_root_active;
