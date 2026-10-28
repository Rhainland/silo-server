-- +goose Up
-- Runtime belongs to the active recording files, rather than a provider's
-- edition. Maintain it at the file-write boundary so scans, missing-file
-- detection, moves and trash deletion all follow the same rule.
-- +goose StatementBegin
CREATE FUNCTION public.refresh_audiobook_runtime(p_content_ids text[])
RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
    IF COALESCE(cardinality(p_content_ids), 0) = 0 THEN
        RETURN;
    END IF;

    -- Lock before aggregating, in a separate statement: a waiter must see
    -- the preceding writer's committed files, not its pre-wait snapshot.
    -- NO KEY UPDATE permits the file and membership foreign-key checks.
    PERFORM 1 FROM public.media_items
    WHERE content_id = ANY(p_content_ids) AND type = 'audiobook'
      AND NOT (7 = ANY(COALESCE(locked_fields, '{}'::integer[])))
    ORDER BY content_id
    FOR NO KEY UPDATE;

    UPDATE public.media_items mi
    SET runtime = files.minutes, updated_at = NOW()
    FROM (
        SELECT content_id,
               GREATEST(ROUND(MAX(root_seconds) / 60.0), 1)::integer AS minutes
        FROM (
            SELECT content_id, SUM(duration) AS root_seconds
            FROM public.media_files
            WHERE content_id = ANY(p_content_ids)
              AND missing_since IS NULL AND duration > 0
            GROUP BY content_id, media_folder_id, canonical_root_path
        ) roots
        GROUP BY content_id
    ) files
    WHERE mi.content_id = files.content_id AND mi.type = 'audiobook'
      AND NOT (7 = ANY(COALESCE(mi.locked_fields, '{}'::integer[])))
      AND mi.runtime IS DISTINCT FROM files.minutes;
    -- No positive-duration copy leaves the stored value alone, including
    -- temporarily missing books. Field 7 is the existing runtime lock.
END;
$$;

CREATE FUNCTION public.audiobook_runtime_media_files_trigger()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    ids text[];
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT array_agg(DISTINCT content_id) INTO ids FROM new_files;
    ELSIF TG_OP = 'DELETE' THEN
        SELECT array_agg(DISTINCT content_id) INTO ids FROM old_files;
    ELSE
        SELECT array_agg(DISTINCT content_id) INTO ids FROM (
            SELECT unnest(ARRAY[o.content_id, n.content_id]) AS content_id
            FROM old_files o JOIN new_files n USING (id)
            WHERE (o.content_id, o.media_folder_id, o.canonical_root_path, o.duration, o.missing_since)
               IS DISTINCT FROM
                  (n.content_id, n.media_folder_id, n.canonical_root_path, n.duration, n.missing_since)
        ) changed;
    END IF;
    PERFORM public.refresh_audiobook_runtime(ids);
    RETURN NULL;
END;
$$;

-- Statement triggers aggregate a multipart write once, after its resulting
-- rows are available. Both old and new items are refreshed on a reassignment.
CREATE TRIGGER audiobook_runtime_files_insert
AFTER INSERT ON public.media_files
REFERENCING NEW TABLE AS new_files
FOR EACH STATEMENT EXECUTE FUNCTION public.audiobook_runtime_media_files_trigger();

CREATE TRIGGER audiobook_runtime_files_update
AFTER UPDATE ON public.media_files
REFERENCING OLD TABLE AS old_files NEW TABLE AS new_files
FOR EACH STATEMENT EXECUTE FUNCTION public.audiobook_runtime_media_files_trigger();

CREATE TRIGGER audiobook_runtime_files_delete
AFTER DELETE ON public.media_files
REFERENCING OLD TABLE AS old_files
FOR EACH STATEMENT EXECUTE FUNCTION public.audiobook_runtime_media_files_trigger();

SELECT public.refresh_audiobook_runtime(array_agg(content_id))
FROM public.media_items WHERE type = 'audiobook';
-- +goose StatementEnd

-- +goose Down
-- The replaced provider runtimes are not kept, so they cannot be restored.
DROP TRIGGER audiobook_runtime_files_delete ON public.media_files;
DROP TRIGGER audiobook_runtime_files_update ON public.media_files;
DROP TRIGGER audiobook_runtime_files_insert ON public.media_files;
DROP FUNCTION public.audiobook_runtime_media_files_trigger();
DROP FUNCTION public.refresh_audiobook_runtime(text[]);
