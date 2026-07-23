import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import type { Library } from "@/api/types";
import { MetadataMatcherQueuesSection } from "./MetadataMatcherQueuesSection";

const mocks = vi.hoisted(() => ({
  useQueues: vi.fn(),
  useDetail: vi.fn(),
  useRetry: vi.fn(),
}));

vi.mock("@/hooks/queries/admin/libraries", () => ({
  useLibraryMetadataMatchQueues: (...args: unknown[]) => mocks.useQueues(...args),
  useLibraryMetadataMatchQueueDetail: (...args: unknown[]) => mocks.useDetail(...args),
  useRetryLibraryMetadataMatchQueue: (...args: unknown[]) => mocks.useRetry(...args),
}));

describe("MetadataMatcherQueuesSection", () => {
  it("renders the structured failure kind with the parked item detail", async () => {
    mocks.useQueues.mockReturnValue({
      data: [
        {
          library_id: 1,
          movie_count: 1,
          series_count: 0,
          raw_file_count: 0,
          total_count: 1,
          pending_count: 0,
          parked_count: 1,
        },
      ],
    });
    mocks.useDetail.mockReturnValue({
      data: {
        movies: [
          {
            media_file_id: 42,
            file_path: "/media/movies/Unknown.mkv",
            state: "parked",
            failure_kind: "candidate_rejected",
            failure_detail: { message: "Score below threshold" },
          },
        ],
        series: [],
        raw_files: [],
      },
    });
    mocks.useRetry.mockReturnValue({ mutate: vi.fn(), isPending: false, variables: undefined });
    const libraries = [{ id: 1, name: "Movies" }] as Library[];

    render(<MetadataMatcherQueuesSection libraries={libraries} />);
    await userEvent.click(screen.getByRole("button", { name: /metadata matcher/i }));
    await userEvent.click(screen.getByText("Movies"));

    expect(screen.getByText("candidate rejected")).toBeInTheDocument();
    expect(screen.getByText("Score below threshold")).toBeInTheDocument();
    expect(screen.getByText("parked")).toBeInTheDocument();
  });
});
