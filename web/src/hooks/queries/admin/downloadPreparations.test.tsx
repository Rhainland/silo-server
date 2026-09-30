import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { makePreparation, makePreparationList } from "@/test/downloadPreparations";
import type { AdminDownloadPreparationList } from "@/api/v2/adminDownloadPreparations";

const KEY = ["admin", "downloadPreparations", "p1"];

const mocks = vi.hoisted(() => ({
  list: vi.fn<() => Promise<AdminDownloadPreparationList>>(),
}));

vi.mock("@/api/client", () => ({
  captureProfileRequestContext: () => ({ profileId: "p1" }),
  StaleApiRequestContextError: class extends Error {},
}));
vi.mock("@/api/v2/adminDownloadPreparations", () => ({
  adminDownloadPreparationsKey: () => KEY,
  listAdminDownloadPreparations: mocks.list,
}));

import {
  ADMIN_DOWNLOAD_PREPARATIONS_ACTIVE_REFRESH,
  useAdminDownloadPreparationsRefresh,
} from "./downloadPreparations";

function setup() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  function wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  }
  renderHook(() => useAdminDownloadPreparationsRefresh(), { wrapper });
  return client;
}

describe("useAdminDownloadPreparationsRefresh", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mocks.list.mockReset();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("re-reads the list on schedule even while progress patches keep arriving", async () => {
    const list = makePreparationList([makePreparation()]);
    mocks.list.mockResolvedValue(list);
    const client = setup();
    await act(async () => {});
    expect(mocks.list).toHaveBeenCalledTimes(1);

    // A progress event patches the cached list every five seconds.
    for (let elapsed = 0; elapsed < ADMIN_DOWNLOAD_PREPARATIONS_ACTIVE_REFRESH; elapsed += 5_000) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(5_000);
        client.setQueryData<AdminDownloadPreparationList>(KEY, (current) =>
          current ? { ...current, items: [...current.items] } : current,
        );
      });
    }
    expect(mocks.list).toHaveBeenCalledTimes(2);
  });

  it("stops re-reading once nothing is in flight", async () => {
    mocks.list.mockResolvedValue(makePreparationList([], { failed_recent: 1 }));
    setup();
    await act(async () => {});
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ADMIN_DOWNLOAD_PREPARATIONS_ACTIVE_REFRESH * 3);
    });
    expect(mocks.list).toHaveBeenCalledTimes(1);
  });
});
