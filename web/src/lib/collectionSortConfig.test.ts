import { describe, expect, it } from "vitest";

import {
  COLLECTION_SOURCE_ORDER,
  selectValueToSortConfig,
  sortConfigToSelectValue,
} from "@/lib/collectionSortConfig";

describe("collection sort config", () => {
  it("round-trips source order as an empty config", () => {
    expect(selectValueToSortConfig(COLLECTION_SOURCE_ORDER)).toEqual({});
    expect(sortConfigToSelectValue({})).toBe(COLLECTION_SOURCE_ORDER);
  });

  it("normalizes an invalid select order to the field default", () => {
    expect(selectValueToSortConfig("title:sideways")).toEqual({
      field: "title",
      order: "asc",
    });
  });
});
