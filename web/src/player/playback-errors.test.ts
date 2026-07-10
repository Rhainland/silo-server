import { describe, expect, it } from "vitest";

import { describeTranscodingPolicyError } from "./playback-errors";
import { PlayerFetchError } from "./player-fetch";

describe("describeTranscodingPolicyError", () => {
  it("describes disabled video transcoding", () => {
    const error = new PlayerFetchError(
      403,
      "Transcoding is disabled for your user",
      "transcoding_disabled",
    );

    expect(describeTranscodingPolicyError(error)).toEqual({
      title: "Transcoding is disabled",
      message: "Transcoding is disabled for your user. Ask your server administrator for access.",
    });
  });

  it("describes disabled audio transcoding", () => {
    const error = new PlayerFetchError(
      403,
      "Audio transcoding is disabled for your user",
      "audio_transcoding_disabled",
    );

    expect(describeTranscodingPolicyError(error)).toEqual({
      title: "Audio transcoding is disabled",
      message:
        "This item requires audio conversion, but audio transcoding is disabled for your user.",
    });
  });

  it("ignores unrelated player errors", () => {
    expect(
      describeTranscodingPolicyError(new PlayerFetchError(500, "Failed", "internal_error")),
    ).toBeNull();
  });
});
