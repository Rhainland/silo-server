-- +goose Up
-- A season's landscape art, following its poster columns: the served path,
-- the provider source the image cache resolves, and a thumbhash. Continue
-- Watching and Next Up show it on episode cards before the series backdrop.
ALTER TABLE public.seasons
    ADD COLUMN IF NOT EXISTS backdrop_path text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS backdrop_source_path text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS backdrop_thumbhash text NOT NULL DEFAULT '';

DROP TRIGGER IF EXISTS seasons_artwork_revision_gc_update ON public.seasons;
DROP TRIGGER IF EXISTS seasons_artwork_revision_gc_delete ON public.seasons;

CREATE TRIGGER seasons_artwork_revision_gc_update
AFTER UPDATE OF poster_path, backdrop_path ON public.seasons
FOR EACH ROW
WHEN (OLD.poster_path IS DISTINCT FROM NEW.poster_path
   OR OLD.backdrop_path IS DISTINCT FROM NEW.backdrop_path)
EXECUTE FUNCTION public.queue_displaced_artwork_revision(
    'poster_path', 'poster', 'backdrop_path', 'backdrop'
);

CREATE TRIGGER seasons_artwork_revision_gc_delete
AFTER DELETE ON public.seasons
FOR EACH ROW EXECUTE FUNCTION public.queue_displaced_artwork_revision(
    'poster_path', 'poster', 'backdrop_path', 'backdrop'
);

-- +goose Down
DROP TRIGGER IF EXISTS seasons_artwork_revision_gc_update ON public.seasons;
DROP TRIGGER IF EXISTS seasons_artwork_revision_gc_delete ON public.seasons;

CREATE TRIGGER seasons_artwork_revision_gc_update
AFTER UPDATE OF poster_path ON public.seasons
FOR EACH ROW
WHEN (OLD.poster_path IS DISTINCT FROM NEW.poster_path)
EXECUTE FUNCTION public.queue_displaced_artwork_revision('poster_path', 'poster');

CREATE TRIGGER seasons_artwork_revision_gc_delete
AFTER DELETE ON public.seasons
FOR EACH ROW EXECUTE FUNCTION public.queue_displaced_artwork_revision('poster_path', 'poster');

ALTER TABLE public.seasons
    DROP COLUMN IF EXISTS backdrop_thumbhash,
    DROP COLUMN IF EXISTS backdrop_source_path,
    DROP COLUMN IF EXISTS backdrop_path;
