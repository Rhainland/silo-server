-- +goose Up
-- An audiobook's runtime is the length of its own files. The scanner now sets
-- it from the probed durations and enrichment no longer copies a provider's
-- runtime, which describes whichever edition the provider matched. Existing
-- rows take the active-file total each library holds; a book in several
-- libraries takes the longest copy. Rounding matches audiobookRuntimeMinutes in
-- internal/scanner: nearest minute, at least one for any known duration.
-- Books with no probed duration keep the runtime they have.
-- +goose StatementBegin
UPDATE public.media_items mi
SET runtime = files.minutes,
    updated_at = NOW()
FROM (
    SELECT content_id,
           GREATEST(ROUND(MAX(duration_seconds) / 60.0), 1)::integer AS minutes
    FROM public.audiobook_item_file_stats
    WHERE active_file_count > 0
      AND duration_seconds > 0
    GROUP BY content_id
) files
WHERE mi.content_id = files.content_id
  AND mi.type = 'audiobook'
  AND mi.runtime IS DISTINCT FROM files.minutes;
-- +goose StatementEnd

-- +goose Down
-- The replaced provider runtimes are not kept, so there is nothing to restore.
SELECT 1;
