import { act, cleanup, renderHook } from "@testing-library/react";
import { useMemo } from "react";
import { afterEach, expect, it, vi } from "vitest";

import { useAdminLogStream } from "./useAdminLogStream";

const { mint } = vi.hoisted(() => ({ mint: vi.fn() }));
vi.mock("@/api/client", () => ({
  captureProfileRequestContext: () => ({ profileId: "admin-profile" }),
  isCapturedProfileAuthorityActive: () => true,
}));
vi.mock("@/api/v2/adminLogsSocket", () => ({
  mintAdminLogsSocketTicket: mint,
  buildAdminLogsSocketQuery: (params: { method: string }) => `method=${params.method}`,
  buildAdminLogsSocketUrl: () => "ws://example.test/logs",
  adminLogsSocketProtocols: () => [],
}));

class FakeWebSocket {
  static OPEN = 1;
  static CONNECTING = 0;
  static instances: FakeWebSocket[] = [];
  readyState = FakeWebSocket.OPEN;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: (() => void) | null = null;
  constructor() {
    FakeWebSocket.instances.push(this);
  }
  close() {
    this.readyState = 3;
  }
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  mint.mockReset();
  FakeWebSocket.instances = [];
});

it("hides previous filter matches after a failed handshake and accepts a fresh snapshot on recovery", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("WebSocket", FakeWebSocket);
  mint.mockResolvedValue({ ticket: "test-ticket" });
  const { result, rerender } = renderHook(
    ({ method }) => {
      const params = useMemo(() => ({ method }), [method]);
      return useAdminLogStream("audit", params, true);
    },
    { initialProps: { method: "PUT" } },
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(250);
  });
  act(() => {
    const socket = FakeWebSocket.instances[0];
    if (!socket) throw new Error("Initial log socket was not created");
    socket.onmessage?.({
      data: JSON.stringify({
        type: "snapshot",
        stream: "audit",
        entries: [{ id: 7, method: "PUT" }],
        next_cursor: "old-page",
      }),
    });
  });
  expect(result.current.rows.map((row) => row.id)).toEqual([7]);
  expect(result.current.nextCursor).toBe("old-page");

  mint.mockRejectedValue(new Error("ticket service unavailable"));
  rerender({ method: "POST" });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(250);
  });
  expect(result.current.error).toBe("Unable to open log stream.");
  expect(result.current.rows).toEqual([]);
  expect(result.current.nextCursor).toBeUndefined();

  mint.mockResolvedValue({ ticket: "replacement-ticket" });
  act(() => result.current.reconnect());
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  act(() => {
    const socket = FakeWebSocket.instances[1];
    if (!socket) throw new Error("Recovery log socket was not created");
    socket.onmessage?.({
      data: JSON.stringify({
        type: "snapshot",
        stream: "audit",
        entries: [{ id: 8, method: "POST" }],
      }),
    });
  });
  expect(result.current.rows.map((row) => row.id)).toEqual([8]);
  expect(result.current.error).toBeUndefined();
});
