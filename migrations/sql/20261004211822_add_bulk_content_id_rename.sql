-- Set-based companion to silo_rename_content_id (20260614120000).
--
-- Re-anchoring a provider-anchored series moves the series and every season
-- and episode whose content_id embeds the old anchor. silo_rename_content_id
-- moves one value per call and walks every soft-reference column each time,
-- so a long-running series would cost one catalog walk per episode.
-- silo_rename_content_ids(from[], to[]) moves all pairs with one UPDATE per
-- column. The column predicate and the trending array sweep mirror
-- silo_rename_content_id; keep the two in lockstep. FK children follow via
-- ON UPDATE CASCADE (20260614120000, 20260925011258).
--
-- The caller guarantees that every target id is free and that no id appears
-- as both a source and a target.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION silo_rename_content_ids(p_from text[], p_to text[])
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    c RECORD;
    i integer;
BEGIN
    IF p_from IS NULL OR p_to IS NULL OR cardinality(p_from) = 0 THEN
        RETURN;
    END IF;
    IF cardinality(p_from) <> cardinality(p_to) THEN
        RAISE EXCEPTION 'silo_rename_content_ids: % sources but % targets',
            cardinality(p_from), cardinality(p_to);
    END IF;

    FOR c IN
        SELECT cl.oid::regclass AS rel, a.attname AS col
        FROM pg_class cl
        JOIN pg_namespace n ON n.oid = cl.relnamespace
        JOIN pg_attribute a ON a.attrelid = cl.oid AND a.attnum > 0 AND NOT a.attisdropped
        JOIN pg_type t ON t.oid = a.atttypid
        WHERE cl.relkind IN ('r', 'p')
          AND n.nspname = 'public'
          AND t.typname IN ('text', 'varchar', 'bpchar')
          AND a.attname IN (
                'media_item_id', 'series_id', 'season_id', 'episode_id', 'content_id',
                'season_content_id', 'episode_content_id', 'library_item_id', 'cover_item',
                'item_id', 'similar_item_id', 'source_item_id'
              )
          AND cl.relname NOT LIKE 'content_id_migration%'
          -- Skip real FK children of the family; ON UPDATE CASCADE moves those.
          AND NOT EXISTS (
                SELECT 1
                FROM pg_constraint con
                JOIN unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
                WHERE con.contype = 'f'
                  AND con.conrelid = cl.oid
                  AND con.confrelid IN ('media_items'::regclass, 'seasons'::regclass, 'episodes'::regclass)
                  AND k.attnum = a.attnum
              )
    LOOP
        EXECUTE format(
            'UPDATE %s AS t SET %I = m.to_id
               FROM unnest($1::text[], $2::text[]) AS m(from_id, to_id)
              WHERE t.%I = m.from_id',
            c.rel, c.col, c.col)
            USING p_from, p_to;
    END LOOP;

    IF to_regclass('public.trending_discover_snapshots') IS NOT NULL THEN
        FOR i IN 1 .. cardinality(p_from) LOOP
            UPDATE trending_discover_snapshots
            SET content_ids = array_replace(content_ids, p_from[i], p_to[i])
            WHERE p_from[i] = ANY(content_ids);
        END LOOP;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS silo_rename_content_ids(text[], text[]);
