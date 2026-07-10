import { PlayerFetchError } from "./player-fetch";

export interface PlaybackPolicyErrorDescription {
  title: string;
  message: string;
}

export function describeTranscodingPolicyError(
  error: unknown,
): PlaybackPolicyErrorDescription | null {
  if (!(error instanceof PlayerFetchError) || error.status !== 403) {
    return null;
  }

  if (error.code === "transcoding_disabled") {
    return {
      title: "Transcoding is disabled",
      message: "Transcoding is disabled for your user. Ask your server administrator for access.",
    };
  }

  if (error.code === "audio_transcoding_disabled") {
    return {
      title: "Audio transcoding is disabled",
      message:
        "This item requires audio conversion, but audio transcoding is disabled for your user.",
    };
  }

  return null;
}
