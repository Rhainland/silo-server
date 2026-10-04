// @vitest-environment jsdom
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";

const mocks = vi.hoisted(() => ({
  useSearchItemMatchCandidates: vi.fn(),
  useApplyItemMatch: vi.fn(),
  useCatalogItemDetail: vi.fn(),
  applyMutate: vi.fn(),
}));

vi.mock("@/hooks/queries/items", () => ({
  useSearchItemMatchCandidates: (...args: unknown[]) => mocks.useSearchItemMatchCandidates(...args),
  useApplyItemMatch: (...args: unknown[]) => mocks.useApplyItemMatch(...args),
}));

vi.mock("@/hooks/queries/catalogRead", () => ({
  useCatalogItemDetail: (...args: unknown[]) => mocks.useCatalogItemDetail(...args),
}));

vi.mock("@/components/ui/dialog", () => ({
  Dialog: ({ children, open }: { children: ReactNode; open: boolean }) =>
    open ? <div>{children}</div> : null,
  DialogContent: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DialogHeader: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DialogTitle: ({ children }: { children: ReactNode }) => <h2>{children}</h2>,
}));

import MatchItemDialog from "./MatchItemDialog";

const item = { content_id: "local-abc", title: "The Office", year: 0, type: "series" as const };

function applyWithResult(contentID: string) {
  const onOpenChange = vi.fn();
  const onReplaced = vi.fn();
  render(<MatchItemDialog item={item} open onOpenChange={onOpenChange} onReplaced={onReplaced} />);
  fireEvent.click(screen.getByTestId("match-candidate"));
  fireEvent.click(screen.getByRole("button", { name: "Apply Match" }));
  const [, options] = mocks.applyMutate.mock.calls[0] as [
    unknown,
    { onSuccess: (result: { content_id: string; updated: boolean }) => void },
  ];
  options.onSuccess({ content_id: contentID, updated: true });
  return { onOpenChange, onReplaced };
}

describe("MatchItemDialog onReplaced", () => {
  beforeEach(() => {
    mocks.applyMutate.mockReset();
    mocks.useSearchItemMatchCandidates.mockReturnValue({
      mutate: vi.fn(),
      isPending: false,
      isSuccess: true,
      data: {
        candidates: [
          {
            title: "The Office",
            year: 2001,
            content_type: "series",
            overview: "",
            image_url: "",
            provider_ids: { tvdb: "78107" },
            sources: ["tvdb"],
            agreement_hints: [],
          },
        ],
      },
    });
    mocks.useApplyItemMatch.mockReturnValue({ mutate: mocks.applyMutate, isPending: false });
    mocks.useCatalogItemDetail.mockReturnValue({ data: undefined, isLoading: false });
  });

  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("reports the new content ID when the match moved the item", () => {
    const { onOpenChange, onReplaced } = applyWithResult("series-tvdb-78107");
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(onReplaced).toHaveBeenCalledWith("series-tvdb-78107");
  });

  it("stays quiet when the item kept its content ID", () => {
    const { onOpenChange, onReplaced } = applyWithResult("local-abc");
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(onReplaced).not.toHaveBeenCalled();
  });
});
