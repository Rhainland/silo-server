import { useCallback, useEffect, useRef, useState } from "react";
import { clampSubtitleSyncMs, useSubtitleSyncSetting } from "@/hooks/queries/subtitleSync";

/** How long the Delay control waits after the last nudge before saving. */
const SAVE_AFTER_MS = 600;

/**
 * The player's subtitle delay in ms (positive shows later), bound to the
 * profile's timing offset so a change here follows the profile to every device.
 *
 * A nudge shows at once from a local draft and is saved after a short pause,
 * so holding the button sends one write instead of one per step. Leaving the
 * player saves a nudge still waiting out the pause.
 */
export function useSubtitleDelay(): [number, (ms: number) => void] {
  const { offsetMs: savedMs, save } = useSubtitleSyncSetting();
  const [draftMs, setDraftMs] = useState<number | null>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const flushRef = useRef<(() => void) | null>(null);

  const setDelayMs = useCallback(
    (ms: number) => {
      const next = clampSubtitleSyncMs(ms);
      setDraftMs(next);
      if (timerRef.current) clearTimeout(timerRef.current);
      const flush = () => {
        timerRef.current = null;
        flushRef.current = null;
        // The save resolves after the settings query has refetched, so the
        // saved value already shows when the draft steps aside. Best effort: a
        // failed save keeps the draft for this session and the next nudge
        // retries, rather than interrupting playback.
        void save(next)
          .then(() => setDraftMs((current) => (current === next ? null : current)))
          .catch(() => undefined);
      };
      flushRef.current = flush;
      timerRef.current = setTimeout(flush, SAVE_AFTER_MS);
    },
    [save],
  );

  useEffect(
    () => () => {
      if (timerRef.current) clearTimeout(timerRef.current);
      flushRef.current?.();
    },
    [],
  );

  return [draftMs ?? savedMs, setDelayMs];
}
