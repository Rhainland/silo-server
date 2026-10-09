import { useCallback, useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { v2 } from "@/api/v2/request";
import type { ItemDetail } from "@/api/types";
import { useMetadataAIStatus } from "@/hooks/queries/metadataAI";

// Give up shimmering after this long; the original text stays and the
// server-side cooldown keeps a failing endpoint from being re-hit per view.
const TRANSLATE_TIMEOUT_MS = 45_000;
const POLL_INTERVAL_MS = 2_000;

/** What the hook needs from a page: a detail document, or a season's episode list. */
export type OnViewTranslationTarget = Pick<
  ItemDetail,
  "content_id" | "pending_translation_language" | "series_id" | "season_number"
>;

/**
 * Viewer-facing on-demand description translation for the detail page.
 *
 * When the detail response carries `pending_translation_language` (the
 * description is not in this profile's language and no localization exists),
 * the server's `metadata_ai.on_view` mode decides the UX:
 *   - "auto": fire the translation on view (once per item+language) and pulse
 *     the description until the refetched detail comes back localized.
 *   - "button": expose a translate trigger for the chip under the overview.
 * Completion is observed as the flag clearing on refetch — the job's first
 * batch translates the item's own overview, so this lands in seconds.
 */
export interface OnViewTranslationOptions {
  /**
   * Called on every poll tick while a translation runs, for surfaces whose
   * data does not live under the catalog item keys (Home sections).
   */
  onPoll?: () => void;
}

export function useOnViewTranslation(
  item: OnViewTranslationTarget | undefined,
  { onPoll }: OnViewTranslationOptions = {},
) {
  const queryClient = useQueryClient();
  const { data: status } = useMetadataAIStatus();
  const mode = status?.on_view ?? "off";

  const contentId = item?.content_id ?? "";
  const pendingLanguage = item?.pending_translation_language ?? "";
  const seriesId = item?.series_id;
  const seasonNumber = item?.season_number;
  const key = contentId && pendingLanguage ? `${contentId}\u0000${pendingLanguage}` : "";

  // The translation in flight, keyed by item and language so a surface that
  // switches items (the Featured hero rotating its slides) never shows one
  // item's progress on another.
  const [running, setRunning] = useState<{ key: string; contentId: string } | null>(null);
  const translating = key !== "" && running?.key === key;
  // Item+language pairs already requested, so auto mode triggers once per
  // view rather than re-firing on every refetch while polling.
  const firedRef = useRef(new Set<string>());
  const onPollRef = useRef(onPoll);
  useEffect(() => {
    onPollRef.current = onPoll;
  });

  const trigger = useCallback(() => {
    if (!key || firedRef.current.has(key)) return;
    firedRef.current.add(key);
    setRunning({ key, contentId });
    v2("POST /api/v2/catalog/items/{id}/translate-description", {
      path: { id: contentId },
      body: { target_language: pendingLanguage },
    }).catch(() => {
      setRunning((current) => (current?.key === key ? null : current));
    });
  }, [contentId, key, pendingLanguage]);

  // Auto mode: translate on view.
  useEffect(() => {
    if (mode === "auto" && pendingLanguage) trigger();
  }, [mode, pendingLanguage, trigger]);

  // While translating, poll; the localized overview replaces the text and
  // clears the pending flag, which ends the shimmer below.
  useEffect(() => {
    if (!running) return;
    const startedAt = Date.now();
    const timer = setInterval(() => {
      if (Date.now() - startedAt > TRANSLATE_TIMEOUT_MS) {
        setRunning(null);
        return;
      }
      // Prefix invalidation covers the per-library detail key variants
      // (["catalog", "items", id, "detail", <libraryId|"default">]).
      void queryClient.invalidateQueries({ queryKey: ["catalog", "items", running.contentId] });
      if (seriesId && typeof seasonNumber === "number") {
        void queryClient.invalidateQueries({
          queryKey: ["catalog", "series", seriesId, "seasons", seasonNumber],
        });
      }
      onPollRef.current?.();
    }, POLL_INTERVAL_MS);
    return () => clearInterval(timer);
  }, [running, queryClient, seasonNumber, seriesId]);

  // The refetched item no longer reports a missing language: done.
  useEffect(() => {
    if (running && running.contentId === contentId && !pendingLanguage) setRunning(null);
  }, [running, contentId, pendingLanguage]);

  return {
    /** Pulse the description text. */
    translating,
    /** Render the explicit translate chip (button mode only). */
    onTranslate: mode === "button" && pendingLanguage && !translating ? trigger : undefined,
  };
}
