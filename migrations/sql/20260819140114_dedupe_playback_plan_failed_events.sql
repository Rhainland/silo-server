-- +goose Up
-- Keep the authoritative accepted-replan failure for each concrete plan
-- attempt when one exists. Older clients may have posted a breadcrumb before
-- or after that row, so timestamp/ID order alone cannot identify the keeper;
-- server-synthesized rows carry the accepted replan_request_id diagnostic.
-- Goose runs this migration in one transaction. Hold writers until the
-- cleanup and unique-index build both finish so a rolling old server cannot
-- insert another duplicate between the two statements.
LOCK TABLE playback_route_events IN SHARE ROW EXCLUSIVE MODE;

-- Preserve the union of client and authoritative diagnostics before removing
-- duplicate rows. Later rows win generally, while the authoritative row wins
-- key conflicts regardless of arrival order.
WITH merged AS (
    SELECT grouped.session_id,
           grouped.plan_attempt_id,
           jsonb_object_agg(entry.key, entry.value ORDER BY grouped.authoritative, grouped.id) AS diagnostics
    FROM (
        SELECT id, session_id, plan_attempt_id, diagnostics,
               diagnostics ? 'replan_request_id' AS authoritative
        FROM playback_route_events
        WHERE event = 'plan_failed'
          AND session_id IS NOT NULL
          AND plan_attempt_id IS NOT NULL
    ) grouped
    CROSS JOIN LATERAL jsonb_each(grouped.diagnostics) entry
    GROUP BY grouped.session_id, grouped.plan_attempt_id
)
UPDATE playback_route_events target
SET diagnostics = merged.diagnostics
FROM merged
WHERE target.event = 'plan_failed'
  AND target.session_id = merged.session_id
  AND target.plan_attempt_id = merged.plan_attempt_id;

DELETE FROM playback_route_events older
USING playback_route_events newer
WHERE older.event = 'plan_failed'
  AND newer.event = 'plan_failed'
  AND older.session_id = newer.session_id
  AND older.plan_attempt_id = newer.plan_attempt_id
  AND (
      ((newer.diagnostics ? 'replan_request_id') AND NOT (older.diagnostics ? 'replan_request_id'))
      OR (
          (newer.diagnostics ? 'replan_request_id') = (older.diagnostics ? 'replan_request_id')
          AND older.id < newer.id
      )
  );

CREATE UNIQUE INDEX playback_route_events_plan_failed_attempt_idx
    ON playback_route_events (session_id, plan_attempt_id)
    WHERE event = 'plan_failed'
      AND session_id IS NOT NULL
      AND plan_attempt_id IS NOT NULL;

-- +goose Down
-- The unique index can be removed, but rows deleted by the Up migration cannot
-- be reconstructed.
DROP INDEX IF EXISTS playback_route_events_plan_failed_attempt_idx;
