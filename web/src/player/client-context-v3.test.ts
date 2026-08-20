import { afterEach, describe, expect, it, vi } from "vitest";

import {
  buildClientCapabilitiesV3,
  buildClientPlaybackContextV3,
  buildDeliveriesV3,
  detectHLSSupport,
  type WebCapabilityProbe,
} from "./client-context-v3";
import type { HDRCapabilitiesV3 } from "./protocol-v3";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("detectHLSSupport", () => {
  it("accepts native HLS when Media Source Extensions are unavailable", () => {
    vi.stubGlobal("MediaSource", undefined);
    vi.stubGlobal("document", {
      createElement: () => ({ canPlayType: () => "maybe" }),
    });

    expect(detectHLSSupport()).toBe(true);
  });

  it("falls back to the hls.js Media Source Extensions probe", () => {
    vi.stubGlobal("document", {
      createElement: () => ({ canPlayType: () => "" }),
    });
    vi.stubGlobal("MediaSource", { isTypeSupported: () => true });

    expect(detectHLSSupport()).toBe(true);
  });
});

describe("buildDeliveriesV3", () => {
  it("advertises the embedded text artifacts rendered by the web player", () => {
    const deliveries = buildDeliveriesV3({
      containers: ["mp4"],
      codecsVideo: ["h264"],
      progressiveCodecsVideo: ["h264"],
      codecsAudio: ["aac"],
      maxResolution: "1080p",
      hdr: false,
      hdrDetails: {
        hdr10: false,
        hdr10_plus: false,
        hlg: false,
        dolby_vision_profiles: [],
        dolby_vision_profile_levels: [],
      },
      hls: true,
      videoDecode: [{ codec: "h264", bit_depths: [8], hardware: false }],
      settled: true,
    });

    for (const delivery of Object.values(deliveries)) {
      expect(delivery.subtitles.embedded_text).toBe(true);
      expect(delivery.subtitles.sidecar_text).toBe(true);
    }
  });
});

describe("structured HDR capabilities", () => {
  const probe: WebCapabilityProbe = {
    containers: ["mp4"],
    codecsVideo: ["hevc"],
    progressiveCodecsVideo: ["hevc"],
    codecsAudio: ["eac3"],
    maxResolution: "2160p",
    hdr: true,
    hdrDetails: {
      hdr10: true,
      hdr10_plus: false,
      hlg: false,
      hdr10_max_width: 3840,
      hdr10_max_height: 2160,
      hdr10_max_frame_rate: 24,
      hdr10_max_bitrate_kbps: 80_000,
      dolby_vision_profiles: [8],
      dolby_vision_profile_levels: [{ profile: 8, max_level: 6, bl_compatibility_ids: [1] }],
    },
    hls: true,
    videoDecode: [],
    settled: true,
  };

  it("publishes the structured formats in both device and active-output contexts", () => {
    expect(buildClientCapabilitiesV3(probe).hdr_details).toEqual(probe.hdrDetails);
    expect(buildClientPlaybackContextV3(probe).output.hdr_details).toEqual(probe.hdrDetails);
  });

  it("publishes software decoder detail without claiming browser hardware", () => {
    const h264Probe: WebCapabilityProbe = {
      ...probe,
      codecsVideo: ["h264"],
      progressiveCodecsVideo: ["h264"],
      videoDecode: [{ codec: "h264", bit_depths: [8], hardware: false }],
    };
    const capabilities = buildClientCapabilitiesV3(h264Probe);
    expect(capabilities.codecs_video_hardware).toEqual([]);
    expect(capabilities.video_decode).toEqual(h264Probe.videoDecode);
  });

  it("scopes normalized HDR sample entries to progressive delivery", () => {
    const deliveries = buildDeliveriesV3(probe);
    expect(deliveries.progressive?.hdr_details).toEqual(probe.hdrDetails);
    const nonProgressiveHDRDetails: HDRCapabilitiesV3 = {
      ...probe.hdrDetails,
      hdr10: false,
      dolby_vision_profiles: [],
      dolby_vision_profile_levels: [],
    };
    delete nonProgressiveHDRDetails.hdr10_max_width;
    delete nonProgressiveHDRDetails.hdr10_max_height;
    delete nonProgressiveHDRDetails.hdr10_max_frame_rate;
    delete nonProgressiveHDRDetails.hdr10_max_bitrate_kbps;
    expect(deliveries.original_http?.hdr_details).toEqual(nonProgressiveHDRDetails);
    expect(deliveries.hls?.hdr_details).toEqual(nonProgressiveHDRDetails);
  });

  it("keeps media-element-only HEVC evidence out of original and HLS delivery", () => {
    const progressiveOnlyProbe = {
      ...probe,
      codecsVideo: ["h264"],
      progressiveCodecsVideo: ["h264", "hevc"],
    };

    expect(buildClientCapabilitiesV3(progressiveOnlyProbe).codecs_video).toEqual(["h264", "hevc"]);
    const deliveries = buildDeliveriesV3(progressiveOnlyProbe);
    expect(deliveries.original_http?.video_codecs).toEqual(["h264"]);
    expect(deliveries.progressive?.video_codecs).toEqual(["h264", "hevc"]);
    expect(deliveries.hls?.video_codecs).toEqual(["h264"]);
  });

  it("scopes exact High 10 MP4 evidence to progressive delivery", () => {
    const high10 = { codec: "h264", profiles: ["high 10"], bit_depths: [10], hardware: false };
    const eightBit = { codec: "h264", bit_depths: [8], hardware: false };
    const deliveries = buildDeliveriesV3({
      ...probe,
      codecsVideo: ["h264"],
      progressiveCodecsVideo: ["h264"],
      videoDecode: [eightBit, high10],
      hlsVideoDecode: [eightBit],
    });

    expect(deliveries.original_http?.video_decode).toEqual([eightBit]);
    expect(deliveries.progressive?.video_decode).toEqual([eightBit, high10]);
    expect(deliveries.hls?.video_decode).toEqual([eightBit]);
  });

  it("keeps every declared decoder represented in delivery-scoped evidence", () => {
    const vp9 = { codec: "vp9", hardware: false };
    const av1 = { codec: "av1", hardware: false };
    const deliveries = buildDeliveriesV3({
      ...probe,
      codecsVideo: ["h264", "vp9", "av1"],
      progressiveCodecsVideo: ["h264", "vp9", "av1"],
      videoDecode: [{ codec: "h264", bit_depths: [8], hardware: false }, vp9, av1],
    });

    expect(deliveries.original_http?.video_decode).toEqual(expect.arrayContaining([vp9, av1]));
    expect(deliveries.progressive?.video_decode).toEqual(expect.arrayContaining([vp9, av1]));
  });
});
