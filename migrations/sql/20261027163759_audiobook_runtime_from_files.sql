-- +goose Up
-- An audiobook's runtime is the length of its own files. The scanner now sets
-- it from the probed durations and enrichment no longer copies a provider's
-- runtime, which describes whichever edition the provider matched. Existing
-- rows take the active-file total of one copy: files are summed per recording
-- root, and a book with several copies (in one library or several) takes the
-- longest, as the scanner does when it writes any one copy. Rounding matches
-- audiobookRuntimeMinutes in internal/scanner: nearest minute, at least one
-- for any known duration. Books with no probed duration keep the runtime they
-- have.
-- +goose StatementBegin
UPDATE public.media_items mi
SET runtime = files.minutes,
    updated_at = NOW()
FROM (
    SELECT content_id,
           GREATEST(ROUND(MAX(root_seconds) / 60.0), 1)::integer AS minutes
    FROM (
        SELECT mf.content_id, SUM(mf.duration) AS root_seconds
        FROM public.media_files mf
        JOIN public.media_items ab
          ON ab.content_id = mf.content_id AND ab.type = 'audiobook'
        WHERE mf.missing_since IS NULL
          AND mf.duration > 0
        GROUP BY mf.content_id, mf.media_folder_id, mf.canonical_root_path
    ) roots
    GROUP BY content_id
) files
WHERE mi.content_id = files.content_id
  AND mi.type = 'audiobook'
  AND mi.runtime IS DISTINCT FROM files.minutes;
-- +goose StatementEnd

-- +goose Down
-- The replaced provider runtimes are not kept, so there is nothing to restore.
SELECT 1;
