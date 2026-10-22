-- +goose Up
-- +goose StatementBegin
-- Denormalized "newest aired episode with a file" on media_items, the
-- "Latest Episode Air Date" sort and rule key (issue #2741). Unlike
-- last_air_date_at (migration 103), which names the show's newest aired
-- episode for the series' last_air_date field, it skips episodes the
-- metadata provider lists but no library has a file for. Membership is
-- episode_libraries, as for latest_episode_added_at. refreshSeriesAirDatesSQL
-- in internal/catalog/episode_repo.go keeps it current.

ALTER TABLE public.media_items
ADD COLUMN IF NOT EXISTS latest_available_episode_air_date_at date;

-- Backfill from existing episodes and their library memberships.
UPDATE public.media_items mi
SET latest_available_episode_air_date_at = sub.last_aired
FROM (
    SELECT e.series_id, MAX(e.air_date) AS last_aired
    FROM public.episodes e
    WHERE e.air_date IS NOT NULL
      AND e.air_date <= CURRENT_DATE
      AND EXISTS (
          SELECT 1 FROM public.episode_libraries el
          WHERE el.episode_id = e.content_id
      )
    GROUP BY e.series_id
) sub
WHERE mi.content_id = sub.series_id
  AND mi.type = 'series';

CREATE INDEX IF NOT EXISTS idx_media_items_latest_available_episode_air_date_at
ON public.media_items USING btree (latest_available_episode_air_date_at DESC NULLS LAST)
WHERE type = 'series';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS public.idx_media_items_latest_available_episode_air_date_at;
ALTER TABLE public.media_items DROP COLUMN IF EXISTS latest_available_episode_air_date_at;
-- +goose StatementEnd
