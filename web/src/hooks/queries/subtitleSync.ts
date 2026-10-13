import { useCallback } from "react";
import { useEffectiveSettings } from "@/hooks/queries/settingValues";
import { useProfileDefaultWriter } from "@/hooks/queries/profileDefaults";
import { SETTING_DEFINITIONS, SETTING_KEYS } from "@/lib/settingsContract";

const KEY = SETTING_KEYS.PLAYER_SUBTITLE_SYNC_MS;
const KEYS = [KEY];

/** The contract's bounds for the offset, so a nudge past them is clamped rather than refused. */
export const SUBTITLE_SYNC_MAX_MS = 10_000;

export function clampSubtitleSyncMs(ms: number): number {
  return Math.max(-SUBTITLE_SYNC_MAX_MS, Math.min(SUBTITLE_SYNC_MAX_MS, Math.round(ms)));
}

/**
 * The profile's subtitle timing offset (player.subtitle_sync_ms), in
 * milliseconds; positive shows subtitles later.
 *
 * Timing is one of the subtitle display settings that follow the profile to
 * every device, so the settings screen and the player's Delay control both
 * write the profile row. The shared writer also clears a row this browser may
 * hold, which would otherwise outrank the profile value here.
 */
export function useSubtitleSyncSetting() {
  const query = useEffectiveSettings({ keys: KEYS });
  const writer = useProfileDefaultWriter(query.data);

  const value = query.data?.[KEY]?.value;
  const offsetMs =
    typeof value === "number" ? value : (SETTING_DEFINITIONS[KEY].defaultValue as number);

  const { save: saveKey, reset: resetKey } = writer;
  const save = useCallback((ms: number) => saveKey(KEY, clampSubtitleSyncMs(ms)), [saveKey]);
  const reset = useCallback(() => resetKey(KEY), [resetKey]);

  return {
    offsetMs,
    /** False until the server's value has arrived, so a caller does not act on the default. */
    isLoaded: query.data !== undefined,
    save,
    reset,
    isSaving: writer.isSaving,
  };
}
