-- +goose Up
-- Access tokens default to 8h. Every database was seeded with 1h, which
-- overrode it and expired the token baked into a web player's media URL every
-- hour of playback. Move only the old seed; any other value is an
-- administrator's choice and stays.
UPDATE server_settings
SET value = '8h'
WHERE key = 'auth.access_token_expiry'
  AND value = '1h';

-- +goose Down
-- Intentionally a no-op: after the update there is no reliable way to
-- distinguish the migrated default from an operator-chosen 8h.
SELECT 1;
