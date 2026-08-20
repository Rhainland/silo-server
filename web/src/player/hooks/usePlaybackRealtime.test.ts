import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PlayerConfigProvider, type PlayerConfig } from "../context/PlayerConfigContext";
import { createPlaybackRealtimeUrlFactory, usePlaybackRealtime } from "./usePlaybackRealtime";

const playerConfig: PlayerConfig = {
  apiBaseUrl: "/api/v1",
  getAccessToken: () => "token",
  getProfileId: () => "profile-1",
  getDeviceId: () => "device-1",
};

function wrapper({ children }: { children: ReactNode }) {
  return createElement(PlayerConfigProvider, { config: playerConfig, children });
}

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("createPlaybackRealtimeUrlFactory", () => {
  it("reads the current token for each reconnect attempt", () => {
    let token: string | null = "stale-token";
    const getUrl = createPlaybackRealtimeUrlFactory("/api/v1", "session-123", () => token);

    expect(getUrl()).toBe("/api/v1/playback/sessions/session-123/control/ws?token=stale-token");

    token = "fresh-token";

    expect(getUrl()).toBe("/api/v1/playback/sessions/session-123/control/ws?token=fresh-token");
  });

  it("keeps a control-socket rejection isolated from media recovery", async () => {
    vi.useFakeTimers();
    const sockets: MockWebSocket[] = [];
    class MockWebSocket extends EventTarget {
      static readonly OPEN = 1;
      static readonly CONNECTING = 0;
      readonly url: string;
      readyState = MockWebSocket.CONNECTING;

      constructor(url: string) {
        super();
        this.url = url;
        sockets.push(this);
      }

      send() {}

      close() {
        this.readyState = 3;
        this.dispatchEvent(new Event("close"));
      }
    }
    vi.stubGlobal("WebSocket", MockWebSocket);
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const onCommand = vi.fn();

    const { result, unmount } = renderHook(
      () => usePlaybackRealtime({ sessionId: "session-1", onCommand }),
      { wrapper },
    );
    expect(sockets).toHaveLength(1);

    act(() => sockets[0]!.dispatchEvent(new Event("error")));
    expect(result.current.connectionState).toBe("disconnected");
    expect(onCommand).not.toHaveBeenCalled();
    expect(fetchSpy).not.toHaveBeenCalled();

    await act(async () => vi.advanceTimersByTimeAsync(500));
    expect(sockets).toHaveLength(2);
    expect(fetchSpy).not.toHaveBeenCalled();
    unmount();
  });

  it("ignores a late command from the socket disposed by a session switch", () => {
    const sockets: MockWebSocket[] = [];
    class MockWebSocket extends EventTarget {
      static readonly OPEN = 1;
      static readonly CONNECTING = 0;
      readyState = MockWebSocket.OPEN;
      constructor(readonly url: string) {
        super();
        sockets.push(this);
      }
      send() {}
      close() {
        this.readyState = 3;
      }
    }
    vi.stubGlobal("WebSocket", MockWebSocket);
    const first = vi.fn();
    const second = vi.fn();
    const { rerender } = renderHook(
      ({ sessionId, onCommand }) => usePlaybackRealtime({ sessionId, onCommand }),
      { wrapper, initialProps: { sessionId: "session-1", onCommand: first } },
    );
    const oldSocket = sockets[0]!;
    rerender({ sessionId: "session-2", onCommand: second });

    act(() =>
      oldSocket.dispatchEvent(
        new MessageEvent("message", {
          data: JSON.stringify({
            type: "command",
            session_id: "session-1",
            command_id: "command-old",
            name: "stop",
          }),
        }),
      ),
    );

    expect(first).not.toHaveBeenCalled();
    expect(second).not.toHaveBeenCalled();
  });
});
