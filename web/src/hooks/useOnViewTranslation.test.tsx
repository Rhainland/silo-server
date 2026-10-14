import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { ItemDetail } from "@/api/types";
import { useOnViewTranslation } from "./useOnViewTranslation";

const v2Mock = vi.hoisted(() => vi.fn());
const statusMock = vi.hoisted(() => vi.fn());

vi.mock("@/api/v2/request", async () => {
  const actual = await vi.importActual<typeof import("@/api/v2/request")>("@/api/v2/request");
  return { ...actual, v2: v2Mock };
});

vi.mock("@/hooks/queries/metadataAI", () => ({
  useMetadataAIStatus: statusMock,
}));

const item = {
  content_id: "movie1",
  pending_translation_language: "de",
} as ItemDetail;

function setup(mode: "auto" | "button") {
  statusMock.mockReturnValue({ data: { on_view: mode } });
  const queryClient = new QueryClient();
  const invalidate = vi.spyOn(queryClient, "invalidateQueries");
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
  const hook = renderHook(() => useOnViewTranslation(item), { wrapper });
  return { hook, invalidate };
}

describe("useOnViewTranslation", () => {
  beforeEach(() => {
    v2Mock.mockReset();
    statusMock.mockReset();
  });

  // Another tab or profile can finish the translation first, and the server
  // then refuses the request: the page re-reads its detail.
  it("re-reads the detail after a refused request", async () => {
    v2Mock.mockRejectedValue(new Error("nothing to translate"));
    const { hook, invalidate } = setup("auto");

    await waitFor(() =>
      expect(invalidate).toHaveBeenCalledWith({ queryKey: ["catalog", "items", "movie1"] }),
    );
    expect(hook.result.current.translating).toBe(false);
    expect(v2Mock).toHaveBeenCalledTimes(1);
  });

  it("lets the button try again after a failed request", async () => {
    v2Mock.mockRejectedValue(new Error("rate limited"));
    const { hook } = setup("button");

    act(() => hook.result.current.onTranslate?.());
    await waitFor(() => expect(hook.result.current.translating).toBe(false));
    act(() => hook.result.current.onTranslate?.());

    expect(v2Mock).toHaveBeenCalledTimes(2);
  });
});
