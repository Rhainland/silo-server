import type { AutoscanSettings, AutoscanSource } from "@/api/types";

/**
 * Whether the header offers Run now. Run now polls every enabled polling
 * source immediately, but it does nothing while Autoscan is off and never
 * polls webhook sources, so it is offered only when Autoscan is on and at
 * least one enabled source polls.
 */
export function showRunNow(
  sources: AutoscanSource[] | undefined,
  settings: AutoscanSettings | undefined,
): boolean {
  if (!settings?.enabled || !sources) return false;
  return sources.some((source) => source.enabled && source.delivery_mode === "poll");
}
