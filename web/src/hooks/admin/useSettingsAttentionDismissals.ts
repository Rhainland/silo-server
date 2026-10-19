import { useCallback, useEffect, useMemo, useSyncExternalStore } from "react";

import { useAuth } from "@/hooks/useAuth";
import { useSettingsAttentionState, type SettingsHealthItem } from "./useSettingsHealth";

const STORAGE_KEY_PREFIX = "silo.admin-dashboard.dismissed-attention.v1";

/** Where this account's dismissals live in localStorage. */
export function settingsAttentionDismissalsKey(userId: number): string {
  return `${STORAGE_KEY_PREFIX}.u${userId}`;
}

/**
 * Whether the admin may hide an item from the Dashboard. Only setup the admin
 * has not done ("Email · Not set up") qualifies: a server can deliberately run
 * without mail or a public URL. A broken service or a restart a saved setting
 * is waiting on stays until it is fixed.
 */
export function isSettingsAttentionDismissible(item: SettingsHealthItem): boolean {
  return item.state === "off";
}

/**
 * What a dismissal remembers. The state text is part of it, so an item that
 * comes back in a different state ("Restart pending" after "Not set up") is
 * shown again rather than staying hidden under the old dismissal.
 */
export function settingsAttentionDismissalId(item: SettingsHealthItem): string {
  return `${item.id}:${item.stateText}`;
}

// localStorage only notifies other tabs, so writes from this tab notify the
// subscribers here themselves.
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

function readRaw(key: string | null): string | null {
  if (key === null) return null;
  try {
    return window.localStorage.getItem(key);
  } catch {
    return null;
  }
}

function parseIds(raw: string | null): string[] {
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed)
      ? parsed.filter((value): value is string => typeof value === "string")
      : [];
  } catch {
    return [];
  }
}

function writeIds(key: string, ids: readonly string[]) {
  try {
    if (ids.length === 0) {
      window.localStorage.removeItem(key);
    } else {
      window.localStorage.setItem(key, JSON.stringify(ids));
    }
  } catch {
    // Storage full or unavailable: the notice simply stays visible.
  }
  for (const listener of listeners) listener();
}

export interface SettingsAttentionDismissals {
  /** The items the Dashboard notice shows. */
  visible: SettingsHealthItem[];
  /** How many current items the admin has hidden. */
  hiddenCount: number;
  dismiss: (item: SettingsHealthItem) => void;
  restoreAll: () => void;
}

/**
 * The Dashboard's attention items with the ones this admin hid taken out.
 *
 * Dismissals are kept per account in this browser. Once every input has
 * loaded, a dismissal whose item no longer needs attention is forgotten, so
 * setup that was done and later undone asks for attention again.
 */
export function useSettingsAttentionDismissals(): SettingsAttentionDismissals {
  const { items, loaded } = useSettingsAttentionState();
  const userId = useAuth().user?.id ?? null;
  const key = userId === null ? null : settingsAttentionDismissalsKey(userId);

  // The raw string is the snapshot: it compares by value, so an unrelated
  // render never sees a "new" list.
  const raw = useSyncExternalStore(
    subscribe,
    () => readRaw(key),
    () => null,
  );
  const dismissed = useMemo(() => new Set(parseIds(raw)), [raw]);

  // Only an item that is gone — resolved — drops its dismissal. One passing
  // through another state keeps it: saving an unrelated playback setting turns
  // "Transcoding · Off" into "Restart pending" until the restart, after which
  // it is "Off" again and should stay hidden.
  useEffect(() => {
    if (key === null || !loaded || dismissed.size === 0) return;
    const current = new Set(items.map((item) => item.id));
    const kept = [...dismissed].filter((id) => current.has(id.slice(0, id.indexOf(":"))));
    if (kept.length !== dismissed.size) writeIds(key, kept);
  }, [dismissed, items, key, loaded]);

  const visible = useMemo(
    () =>
      items.filter(
        (item) =>
          !isSettingsAttentionDismissible(item) ||
          !dismissed.has(settingsAttentionDismissalId(item)),
      ),
    [dismissed, items],
  );

  const dismiss = useCallback(
    (item: SettingsHealthItem) => {
      if (key === null || !isSettingsAttentionDismissible(item)) return;
      const ids = parseIds(readRaw(key));
      const id = settingsAttentionDismissalId(item);
      if (!ids.includes(id)) writeIds(key, [...ids, id]);
    },
    [key],
  );

  const restoreAll = useCallback(() => {
    if (key !== null) writeIds(key, []);
  }, [key]);

  return { visible, hiddenCount: items.length - visible.length, dismiss, restoreAll };
}
