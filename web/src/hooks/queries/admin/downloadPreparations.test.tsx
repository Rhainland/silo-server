import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { makePreparation, makePreparationList } from "@/test/downloadPreparations";
import type { AdminDownloadPreparationList } from "@/api/v2/adminDownloadPreparations";

const mocks = vi.hoisted(() => ({
  list: vi.fn<() => Promise<AdminDownloadPreparationList>>(),
}));

vi.mock("@/api/client", () => ({
  captureProfileRequestContext: () => ({ profileId: "p1" }),
  StaleApiRequestContextError: class extends Error {},
}));
vi.mock("@/api/v2/adminDownloadPreparations", () => ({
  adminDownloadPreparationsKey: () => ["admin", "downloadPreparations", "p1"],
  listAdminDownloadPreparations: mocks.list,
}));

import {
  ADMIN_DOWNLOAD_PREPARATIONS_ACTIVE_REFRESH,
  useAdminDownloadPreparations,
} from "./downloadPreparations";

function wrapper({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

describe("useAdminDownloadPreparations", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mocks.list.mockReset();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("re-reads the list while jobs are active", async () => {
    mocks.list.mockResolvedValue(makePreparationList([makePreparation()]));
    renderHook(() => useAdminDownloadPreparations(), { wrapper });
    await act(async () => {});
    expect(mocks.list).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ADMIN_DOWNLOAD_PREPARATIONS_ACTIVE_REFRESH);
    });
    expect(mocks.list).toHaveBeenCalledTimes(2);
  });

  it("stops re-reading once nothing is in flight", async () => {
    mocks.list.mockResolvedValue(makePreparationList([], { failed_recent: 1 }));
    renderHook(() => useAdminDownloadPreparations(), { wrapper });
    await act(async () => {});
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ADMIN_DOWNLOAD_PREPARATIONS_ACTIVE_REFRESH * 3);
    });
    expect(mocks.list).toHaveBeenCalledTimes(1);
  });
});
