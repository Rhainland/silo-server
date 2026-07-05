import { createContext, useCallback, useContext, useEffect, useSyncExternalStore } from "react";
import type { ReactNode } from "react";

import {
  getDateTimeFormatPreferences,
  parseDateFormatPreference,
  parseTimeFormatPreference,
  setDateTimeFormatPreferences,
  subscribeDateTimeFormatPreferences,
} from "@/lib/datetime";
import type {
  DateFormatPreference,
  DateTimeFormatPreferences,
  TimeFormatPreference,
} from "@/lib/datetime";
import { useSettings, useSetSetting } from "@/hooks/queries/settings";
import { useOptionalAuth } from "@/hooks/useAuth";
import { storage } from "@/utils/storage";

export const DATE_FORMAT_SETTING_KEY = "ui.date_format";
export const TIME_FORMAT_SETTING_KEY = "ui.time_format";

// Seed the shared formatter state from localStorage at module load, before the
// first render, so an app booting straight into a date-heavy page doesn't
// paint in the wrong format while the settings request is in flight.
setDateTimeFormatPreferences({
  dateFormat: parseDateFormatPreference(storage.get(storage.KEYS.UI_DATE_FORMAT)),
  timeFormat: parseTimeFormatPreference(storage.get(storage.KEYS.UI_TIME_FORMAT)),
});

interface DateTimeFormatContextValue {
  dateFormat: DateFormatPreference;
  timeFormat: TimeFormatPreference;
  setDateFormat: (value: DateFormatPreference) => void;
  setTimeFormat: (value: TimeFormatPreference) => void;
}

const DateTimeFormatContext = createContext<DateTimeFormatContextValue | null>(null);

/**
 * Syncs the persisted date/time format settings (profile-scoped, mirrored in
 * localStorage like the theme preferences) into the shared formatter state in
 * lib/datetime, and exposes setters for the settings UI.
 */
export function DateTimeFormatProvider({ children }: { children: ReactNode }) {
  const auth = useOptionalAuth();
  const loadApiSettings = Boolean(auth && !auth.loading && auth.user);
  const { data: apiSettings } = useSettings({ enabled: loadApiSettings });
  const settingMutation = useSetSetting();

  const local = useDateTimeFormat();
  // Once the authenticated settings have loaded they are authoritative: a
  // missing key means the user has no preference (auto), not "fall back to
  // whatever this device saw last" — localStorage only bridges the gap until
  // the request resolves, and rollback of a failed save flows back through the
  // query cache. Local state covers logged-out rendering.
  const apiLoaded = loadApiSettings && apiSettings !== undefined;
  const dateFormat = apiLoaded
    ? parseDateFormatPreference(apiSettings[DATE_FORMAT_SETTING_KEY])
    : local.dateFormat;
  const timeFormat = apiLoaded
    ? parseTimeFormatPreference(apiSettings[TIME_FORMAT_SETTING_KEY])
    : local.timeFormat;

  useEffect(() => {
    setDateTimeFormatPreferences({ dateFormat, timeFormat });
    // Mirror the resolved values so the next load on this device paints in the
    // right format before the settings request resolves.
    if (apiLoaded) {
      storage.set(storage.KEYS.UI_DATE_FORMAT, dateFormat);
      storage.set(storage.KEYS.UI_TIME_FORMAT, timeFormat);
    }
  }, [dateFormat, timeFormat, apiLoaded]);

  const setDateFormat = useCallback(
    (value: DateFormatPreference) => {
      setDateTimeFormatPreferences({ ...getDateTimeFormatPreferences(), dateFormat: value });
      storage.set(storage.KEYS.UI_DATE_FORMAT, value);
      settingMutation.mutate({ key: DATE_FORMAT_SETTING_KEY, value });
    },
    [settingMutation],
  );

  const setTimeFormat = useCallback(
    (value: TimeFormatPreference) => {
      setDateTimeFormatPreferences({ ...getDateTimeFormatPreferences(), timeFormat: value });
      storage.set(storage.KEYS.UI_TIME_FORMAT, value);
      settingMutation.mutate({ key: TIME_FORMAT_SETTING_KEY, value });
    },
    [settingMutation],
  );

  return (
    <DateTimeFormatContext
      value={{
        dateFormat,
        timeFormat,
        setDateFormat,
        setTimeFormat,
      }}
    >
      {children}
    </DateTimeFormatContext>
  );
}

/** Settings-page access to the persisted preferences and their setters. */
export function useDateTimeFormatSettings(): DateTimeFormatContextValue {
  const ctx = useContext(DateTimeFormatContext);
  if (!ctx) {
    throw new Error("useDateTimeFormatSettings must be used within DateTimeFormatProvider");
  }
  return ctx;
}

/**
 * Subscribe to the current date/time format preferences. Components that
 * render dates via lib/datetime formatters should call this so they re-render
 * when the preference changes.
 */
export function useDateTimeFormat(): DateTimeFormatPreferences {
  return useSyncExternalStore(
    subscribeDateTimeFormatPreferences,
    getDateTimeFormatPreferences,
    getDateTimeFormatPreferences,
  );
}
