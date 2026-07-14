-- +goose Up
CREATE TABLE public.artwork_revision_gc_candidates (
    id bigserial PRIMARY KEY,
    original_path text NOT NULL UNIQUE,
    object_keys text[] NOT NULL,
    not_before timestamptz NOT NULL,
    -- NULL means the revision is currently referenced and dormant. Artwork
    -- displacement triggers reactivate it without losing the exact manifest.
    next_attempt_at timestamptz,
    attempt_count integer NOT NULL DEFAULT 0,
    locked_at timestamptz,
    locked_by text NOT NULL DEFAULT '',
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT NOW(),
    updated_at timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT artwork_revision_gc_original_path_check CHECK (BTRIM(original_path) <> ''),
    CONSTRAINT artwork_revision_gc_object_keys_check CHECK (cardinality(object_keys) > 0),
    CONSTRAINT artwork_revision_gc_attempt_count_check CHECK (attempt_count >= 0)
);

CREATE INDEX artwork_revision_gc_due_idx
    ON public.artwork_revision_gc_candidates (next_attempt_at, id)
    WHERE locked_at IS NULL AND next_attempt_at IS NOT NULL;

CREATE INDEX artwork_revision_gc_lease_idx
    ON public.artwork_revision_gc_candidates (locked_at, id)
    WHERE locked_at IS NOT NULL;

-- Queue displaced immutable artwork at the database boundary so every writer
-- participates in the same lifecycle, including background refreshes, scanners,
-- localizations, and future code paths that do not use the admin publication API.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.queue_displaced_artwork_revision()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    arg_index integer;
    path_column text;
    image_type text;
    previous_path text;
    replacement_path text;
    object_keys text[];
    cleanup_at timestamptz := NOW() + interval '24 hours';
BEGIN
    arg_index := 0;
    WHILE arg_index < TG_NARGS LOOP
        path_column := TG_ARGV[arg_index];
        image_type := TG_ARGV[arg_index + 1];
        previous_path := to_jsonb(OLD) ->> path_column;
        replacement_path := CASE WHEN TG_OP = 'UPDATE' THEN to_jsonb(NEW) ->> path_column ELSE NULL END;

        IF COALESCE(BTRIM(previous_path), '') <> ''
           AND previous_path NOT LIKE '%://%'
           AND previous_path ~ '/original\.[^/]+$'
           AND (TG_OP = 'DELETE' OR previous_path IS DISTINCT FROM replacement_path) THEN
            object_keys := CASE image_type
                WHEN 'backdrop' THEN ARRAY[
                    previous_path,
                    regexp_replace(previous_path, '/original(\.[^/]*)$', '/w1920\1'),
                    regexp_replace(previous_path, '/original(\.[^/]*)$', '/w1280\1'),
                    regexp_replace(previous_path, '/original(\.[^/]*)$', '/w300\1')
                ]
                WHEN 'logo' THEN ARRAY[
                    previous_path,
                    regexp_replace(previous_path, '/original(\.[^/]*)$', '/w500\1')
                ]
                ELSE ARRAY[
                    previous_path,
                    regexp_replace(previous_path, '/original(\.[^/]*)$', '/w500\1'),
                    regexp_replace(previous_path, '/original(\.[^/]*)$', '/w300\1')
                ]
            END;

            INSERT INTO public.artwork_revision_gc_candidates (
                original_path, object_keys, not_before, next_attempt_at
            ) VALUES (previous_path, object_keys, cleanup_at, cleanup_at)
            ON CONFLICT (original_path) DO UPDATE SET
                object_keys = artwork_revision_gc_candidates.object_keys,
                not_before = EXCLUDED.not_before,
                next_attempt_at = EXCLUDED.next_attempt_at,
                attempt_count = 0,
                locked_at = NULL,
                locked_by = '',
                last_error = '',
                updated_at = NOW();
        END IF;

        arg_index := arg_index + 2;
    END LOOP;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER media_items_artwork_revision_gc
AFTER UPDATE OF poster_path, backdrop_path, logo_path OR DELETE ON public.media_items
FOR EACH ROW EXECUTE FUNCTION public.queue_displaced_artwork_revision(
    'poster_path', 'poster', 'backdrop_path', 'backdrop', 'logo_path', 'logo'
);

CREATE TRIGGER media_item_localizations_artwork_revision_gc
AFTER UPDATE OF poster_path, backdrop_path, logo_path OR DELETE ON public.media_item_localizations
FOR EACH ROW EXECUTE FUNCTION public.queue_displaced_artwork_revision(
    'poster_path', 'poster', 'backdrop_path', 'backdrop', 'logo_path', 'logo'
);

CREATE TRIGGER seasons_artwork_revision_gc
AFTER UPDATE OF poster_path OR DELETE ON public.seasons
FOR EACH ROW EXECUTE FUNCTION public.queue_displaced_artwork_revision('poster_path', 'poster');

CREATE TRIGGER season_localizations_artwork_revision_gc
AFTER UPDATE OF poster_path OR DELETE ON public.season_localizations
FOR EACH ROW EXECUTE FUNCTION public.queue_displaced_artwork_revision('poster_path', 'poster');

CREATE TRIGGER episodes_artwork_revision_gc
AFTER UPDATE OF still_path OR DELETE ON public.episodes
FOR EACH ROW EXECUTE FUNCTION public.queue_displaced_artwork_revision('still_path', 'still');

CREATE TRIGGER people_artwork_revision_gc
AFTER UPDATE OF photo_path OR DELETE ON public.people
FOR EACH ROW EXECUTE FUNCTION public.queue_displaced_artwork_revision('photo_path', 'profile');

-- +goose Down
DROP TRIGGER IF EXISTS people_artwork_revision_gc ON public.people;
DROP TRIGGER IF EXISTS episodes_artwork_revision_gc ON public.episodes;
DROP TRIGGER IF EXISTS season_localizations_artwork_revision_gc ON public.season_localizations;
DROP TRIGGER IF EXISTS seasons_artwork_revision_gc ON public.seasons;
DROP TRIGGER IF EXISTS media_item_localizations_artwork_revision_gc ON public.media_item_localizations;
DROP TRIGGER IF EXISTS media_items_artwork_revision_gc ON public.media_items;
DROP FUNCTION IF EXISTS public.queue_displaced_artwork_revision();
DROP TABLE IF EXISTS public.artwork_revision_gc_candidates;
