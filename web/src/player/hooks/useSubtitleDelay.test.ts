import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useSubtitleDelay } from "./useSubtitleDelay";

const sync = vi.hoisted(() => ({ offsetMs: 0, save: vi.fn() }));
vi.mock("@/hooks/queries/subtitleSync", async () => {
  const actual = await vi.importActual<typeof import("@/hooks/queries/subtitleSync")>(
    "@/hooks/queries/subtitleSync",
  );
  return {
    ...actual,
    useSubtitleSyncSetting: () => ({ offsetMs: sync.offsetMs, save: sync.save }),
  };
});

describe("useSubtitleDelay", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    sync.offsetMs = 200;
    sync.save.mockReset().mockResolvedValue(undefined);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("starts from the profile's saved offset", () => {
    const { result } = renderHook(() => useSubtitleDelay());
    expect(result.current[0]).toBe(200);
  });

  it("shows each nudge at once and saves only the last one after a pause", () => {
    const { result } = renderHook(() => useSubtitleDelay());

    act(() => result.current[1](300));
    act(() => result.current[1](400));
    expect(result.current[0]).toBe(400);
    expect(sync.save).not.toHaveBeenCalled();

    act(() => vi.advanceTimersByTime(600));
    expect(sync.save).toHaveBeenCalledTimes(1);
    expect(sync.save).toHaveBeenCalledWith(400);
  });

  it("clamps to the contract's bounds", () => {
    const { result } = renderHook(() => useSubtitleDelay());
    act(() => result.current[1](-12_000));
    expect(result.current[0]).toBe(-10_000);
  });

  it("saves a nudge that is still waiting when the player closes", () => {
    const { result, unmount } = renderHook(() => useSubtitleDelay());
    act(() => result.current[1](500));

    unmount();

    expect(sync.save).toHaveBeenCalledWith(500);
  });

  it("keeps the nudged value for this session when the save fails", async () => {
    sync.save.mockRejectedValue(new Error("offline"));
    const { result } = renderHook(() => useSubtitleDelay());

    act(() => result.current[1](700));
    await act(async () => {
      vi.advanceTimersByTime(600);
      await Promise.resolve();
    });

    expect(result.current[0]).toBe(700);
  });
});
