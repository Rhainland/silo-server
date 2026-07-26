-- +goose Up
ALTER TABLE stale_media_ids
    DROP CONSTRAINT stale_media_ids_pkey,
    ADD PRIMARY KEY (content_id, provider, provider_id);

-- +goose Down
WITH ranked AS (
    SELECT ctid,
           ROW_NUMBER() OVER (
               PARTITION BY content_id, provider
               ORDER BY last_seen_at DESC, first_seen_at ASC, provider_id ASC
           ) AS row_number
    FROM stale_media_ids
)
DELETE FROM stale_media_ids
USING ranked
WHERE stale_media_ids.ctid = ranked.ctid
  AND ranked.row_number > 1;

ALTER TABLE stale_media_ids
    DROP CONSTRAINT stale_media_ids_pkey,
    ADD PRIMARY KEY (content_id, provider);
