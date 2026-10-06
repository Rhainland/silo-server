import { describe, expect, it } from "vitest";

import type { AdminUserSettingEntry } from "@/hooks/queries/admin/users";
import { deviceInheritedValue, variesScopePhrase } from "@/lib/inheritedSettingValue";

function entry(
  key: string,
  value: string,
  scope: AdminUserSettingEntry["scope"] = "profile",
  extra: Partial<AdminUserSettingEntry> = {},
): AdminUserSettingEntry {
  return { key, value, scope, profile_id: "alex", ...extra };
}

function inherited(key: string, all: AdminUserSettingEntry[]) {
  return deviceInheritedValue(
    key,
    "Alex",
    all.filter((row) => row.scope === "profile"),
    all,
  );
}

describe("deviceInheritedValue", () => {
  it("uses the profile's own value when the device stores none", () => {
    expect(inherited("playback.audio_language", [entry("playback.audio_language", "fr")])).toEqual({
      profileName: "Alex",
      display: "French",
      raw: "fr",
    });
  });

  it("falls back to the app default when the profile stores nothing either", () => {
    expect(inherited("playback.auto_play_next", [])).toMatchObject({
      profileName: null,
      raw: "true",
    });
  });

  // Library and series settings belong to titles, not devices: they neither
  // answer for the device nor make its value "vary".
  it("ignores library and series values", () => {
    const result = inherited("playback.audio_language", [
      entry("playback.audio_language", "ja", "profile_library", { library_id: 3 }),
      entry("playback.audio_language", "de", "profile_series", { series_id: "s1" }),
    ]);
    expect(result.profileName).toBeNull();
    expect(result.orVaries).toBeUndefined();
  });

  it("goes straight to the default for a key only a device can set", () => {
    // No profile scope in player.hdr_enabled's resolution order, so a stray
    // profile row (or one from another key) is never what the device uses.
    const result = inherited("player.hdr_enabled", [entry("player.hdr_enabled", "false")]);
    expect(result).toMatchObject({ profileName: null, raw: "true" });
  });

  it("follows a key whose profile value outranks the device's", () => {
    expect(inherited("ui.title_art", [entry("ui.title_art", "false")])).toMatchObject({
      profileName: "Alex",
      raw: "false",
    });
  });

  it("notes an app-family value that can apply before the profile's", () => {
    const result = inherited("ui.card_presentation", [
      entry("ui.card_presentation", "{}", "profile_client", { client_family: "tv" }),
    ]);
    expect(result.orVaries).toBe("app family");
  });

  it("only reads the profile's rows it is given", () => {
    expect(
      deviceInheritedValue(
        "playback.audio_language",
        "Alex",
        [],
        [entry("playback.audio_language", "fr", "profile", { profile_id: "sam" })],
      ).profileName,
    ).toBeNull();
  });
});

describe("variesScopePhrase", () => {
  it("picks the article from the scope's first letter", () => {
    expect(variesScopePhrase("app family")).toBe("an app family");
    expect(variesScopePhrase("device")).toBe("a device");
    expect(variesScopePhrase("library")).toBe("a library");
  });
});
