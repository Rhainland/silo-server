// @vitest-environment jsdom

import { render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { describe, expect, it, vi } from "vitest";

import type { AdminDeviceSetting, AdminUserSettingEntry } from "@/hooks/queries/admin/users";

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
if (typeof globalThis.ResizeObserver === "undefined") {
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserverStub }).ResizeObserver =
    ResizeObserverStub;
}

const profiles = [
  {
    profile_id: "alex",
    profile_name: "Alex",
    override_count: 1,
    last_updated: "2026-10-01T00:00:00Z",
  },
];
const device = {
  user_id: 1,
  username: "admin",
  email: "admin@example.test",
  device_id: "tv-1",
  device_name: "Living Room TV",
  device_platform: "tvOS",
  override_count: 1,
  profile_count: 1,
  profiles,
  last_updated: "2026-10-01T00:00:00Z",
};
const hdrOverride: AdminDeviceSetting = {
  user_id: 1,
  profile_id: "alex",
  profile_name: "Alex",
  device_id: "tv-1",
  device_name: "Living Room TV",
  device_platform: "tvOS",
  key: "player.hdr_enabled",
  value: "false",
  updated_at: "2026-10-01T00:00:00Z",
};
// What GET /admin/users/1/settings/values returns for this account: the
// device's own HDR choice, and the profile's audio language.
const stored: AdminUserSettingEntry[] = [
  {
    key: "player.hdr_enabled",
    value: "false",
    scope: "profile_device",
    profile_id: "alex",
    device_id: "tv-1",
  },
  { key: "playback.audio_language", value: "fr", scope: "profile", profile_id: "alex" },
];

vi.mock("@/hooks/queries/admin/users", () => ({
  useAdminDevices: () => ({ data: [device], isLoading: false }),
  useAdminDeviceDetail: () => ({ data: { ...device, settings: [] }, isLoading: false }),
  useAdminDeviceOverrides: () => ({ data: [hdrOverride], isLoading: false, isError: false }),
  useAdminUserSettings: () => ({ data: stored, isLoading: false, isError: false }),
  useUpdateAdminUserDeviceSetting: () => ({ mutate: vi.fn(), isPending: false }),
  useDeleteAdminUserDeviceSetting: () => ({ mutate: vi.fn(), isPending: false }),
  useDeleteAllAdminUserDeviceSettingsForDevice: () => ({ mutate: vi.fn(), isPending: false }),
}));

import AdminDevices from "./AdminDevices";

function rowFor(key: string): HTMLElement {
  return screen.getByText(key, { exact: true }).parentElement!.parentElement as HTMLElement;
}

describe("AdminDevices device detail", () => {
  it("shows what each setting resolves to on the device and where it comes from", () => {
    render(
      <MemoryRouter initialEntries={["/admin/devices/1/tv-1"]}>
        <Routes>
          <Route path="/admin/devices/:userId/:deviceId" element={<AdminDevices />} />
        </Routes>
      </MemoryRouter>,
    );

    // The profile's French, not "default · (empty)".
    const audio = rowFor("playback.audio_language");
    expect(within(audio).getByText("profile")).toBeInTheDocument();
    expect(within(audio).getByText(/from Alex's profile/).parentElement).toHaveTextContent(
      "French · from Alex's profile",
    );

    // The device's own override is still shown as one.
    const hdr = rowFor("player.hdr_enabled");
    expect(within(hdr).getByLabelText("overridden")).toBeInTheDocument();
    expect(within(hdr).getByRole("switch")).not.toBeChecked();

    // Nobody set this one.
    const autoplay = rowFor("playback.auto_play_next");
    expect(within(autoplay).getByText("default")).toBeInTheDocument();
    expect(within(autoplay).getByText(/app default/)).toBeInTheDocument();
  });
});
