package playback

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareSubtitleFilterInputCreatesParserSafeAlias(t *testing.T) {
	outputDir := t.TempDir()
	inputPath := "/media/I'm here [1080p].mkv"
	opts := TranscodeOpts{
		InputPath:          inputPath,
		OutputDir:          outputDir,
		SubtitleBurnIn:     true,
		SubtitleTrackIndex: 2,
		SubtitleCodec:      "subrip",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
	}

	if err := prepareSubtitleFilterInput(&opts); err != nil {
		t.Fatalf("prepareSubtitleFilterInput() error = %v", err)
	}
	wantAlias := filepath.Join(outputDir, subtitleFilterAliasName)
	if opts.subtitleFilterInputPath != wantAlias {
		t.Fatalf("subtitleFilterInputPath = %q, want %q", opts.subtitleFilterInputPath, wantAlias)
	}
	target, err := os.Readlink(wantAlias)
	if err != nil {
		t.Fatalf("read subtitle filter alias: %v", err)
	}
	if target != inputPath {
		t.Fatalf("subtitle filter alias target = %q, want %q", target, inputPath)
	}

	joined := strings.Join(buildFFmpegArgs(opts), " ")
	if !strings.Contains(joined, "-i "+inputPath) {
		t.Fatalf("media input should keep its original path: %s", joined)
	}
	if !strings.Contains(joined, "subtitles='"+wantAlias+"':si=2") {
		t.Fatalf("subtitle filter should use the parser-safe alias: %s", joined)
	}
}

func TestStartTranscodeRejectsUnvalidatedBitstreamFilter(t *testing.T) {
	_, err := StartTranscode(context.Background(), TranscodeOpts{
		VideoBitstreamFilter: "arbitrary_filter=1",
		TargetCodecVideo:     "copy",
	})
	if err == nil {
		t.Fatal("unvalidated bitstream filter was accepted")
	}
	_, err = StartTranscode(context.Background(), TranscodeOpts{
		VideoBitstreamFilter: DV7ToHDR10BitstreamFilter,
		TargetCodecVideo:     "h264",
	})
	if err == nil {
		t.Fatal("DV copy filter was accepted for encoded video")
	}
}

func TestBuildFFmpegArgs_QSVDropsSuperfastPreset(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:         "/media/movie.mkv",
		OutputDir:         "/tmp/out",
		SessionID:         "session-1",
		TargetCodecVideo:  "h264",
		TargetCodecAudio:  "aac",
		SegmentDuration:   2,
		HWAccel:           "qsv",
		FastStart:         true,
		TargetResolution:  "1080p",
		TargetBitrateKbps: 2000,
	})

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-preset superfast") {
		t.Fatalf("QSV args should not use superfast preset: %s", joined)
	}
	if !strings.Contains(joined, "-preset veryfast") {
		t.Fatalf("QSV args should use veryfast preset: %s", joined)
	}
}

func TestBuildFFmpegArgs_CPUPreservesSuperfastFastStart(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:        "/media/movie.mkv",
		OutputDir:        "/tmp/out",
		SessionID:        "session-1",
		TargetCodecVideo: "h264",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
		HWAccel:          "none",
		FastStart:        true,
		TargetResolution: "1080p",
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-preset superfast") {
		t.Fatalf("CPU args should preserve superfast preset: %s", joined)
	}
}

func TestBuildFFmpegArgsBoundsHLSManifestSize(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:        "/media/long.mkv",
		OutputDir:        "/tmp/out",
		SessionID:        "session-long",
		TargetCodecVideo: "h264",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
		TotalDuration:    1_000_000,
	})

	joined := strings.Join(args, " ")
	want := "-hls_list_size 50000"
	if !strings.Contains(joined, want) {
		t.Fatalf("FFmpeg args missing %q: %s", want, joined)
	}
}

func TestBuildFFmpegArgs_CopyVideoFromStartUsesZeroBasedTimestamps(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:        "/media/movie.mkv",
		OutputDir:        "/tmp/out",
		SessionID:        "session-copy",
		TargetCodecVideo: "copy",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
	})

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-copyts") {
		t.Fatalf("copy-video from-start should not preserve source timestamps: %s", joined)
	}
	if !strings.Contains(joined, "-avoid_negative_ts make_zero") {
		t.Fatalf("copy-video from-start should zero-base timestamps: %s", joined)
	}
}

func TestBuildFFmpegArgs_CopyVideoAppliesValidatedBitstreamFilter(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:            "/media/movie.mkv",
		OutputDir:            "/tmp/out",
		SessionID:            "session-dv7",
		TargetCodecVideo:     "copy",
		TargetCodecAudio:     "copy",
		VideoBitstreamFilter: DV7ToHDR10BitstreamFilter,
		SegmentDuration:      2,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c:v copy -bsf:v dovi_rpu=strip=1") {
		t.Fatalf("copy-video args should apply the validated DV bitstream filter: %s", joined)
	}
}

func TestBuildFFmpegArgs_CopyVideoResumePreservesSourceTimestamps(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          "/tmp/out",
		SessionID:          "session-copy-resume",
		SeekSeconds:        478.0,
		StartSegmentNumber: 239,
		TargetCodecVideo:   "copy",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
	})

	joined := strings.Join(args, " ")
	// Resume must preserve source timestamps so TFDT in seg_K matches
	// playlist time K*segDur (the EXT-X-START anchor). Without -copyts,
	// strict players (ATV / ExoPlayer) treat the TFDT/playlist mismatch
	// as a discontinuity and abort.
	if !strings.Contains(joined, "-copyts") {
		t.Fatalf("copy-video resume should preserve source timestamps: %s", joined)
	}
	if !strings.Contains(joined, "-avoid_negative_ts disabled") {
		t.Fatalf("copy-video resume should disable negative-ts adjustment: %s", joined)
	}
	if strings.Contains(joined, "-avoid_negative_ts make_zero") {
		t.Fatalf("copy-video resume must not zero-base timestamps (ATV resume regression): %s", joined)
	}
}

func TestBuildFFmpegArgs_CopyVideoSeekPreservesCodecCopy(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          "/tmp/out",
		SessionID:          "session-copy-seek",
		SeekSeconds:        240.86,
		StartSegmentNumber: 120,
		TargetCodecVideo:   "copy",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
	})

	joined := strings.Join(args, " ")

	// Video must remain copy — no re-encoding.
	if !strings.Contains(joined, "-c:v copy") {
		t.Fatalf("copy-mode seek should preserve -c:v copy: %s", joined)
	}
	// Must not contain any video encoder.
	for _, enc := range []string{"h264_qsv", "h264_vaapi", "h264_nvenc", "libx264", "hevc_qsv", "hevc_nvenc"} {
		if strings.Contains(joined, enc) {
			t.Fatalf("copy-mode seek should not use encoder %s: %s", enc, joined)
		}
	}
	// Seek must be before input.
	ssIdx := strings.Index(joined, "-ss")
	iIdx := strings.Index(joined, "-i ")
	if ssIdx < 0 || iIdx < 0 || ssIdx > iIdx {
		t.Fatalf("seek (-ss) should appear before input (-i): %s", joined)
	}
	// Audio should be transcoded to AAC.
	if !strings.Contains(joined, "-c:a aac") {
		t.Fatalf("copy-mode seek should transcode audio to AAC: %s", joined)
	}
	// Should use -noaccurate_seek for copy video + transcode audio.
	if !strings.Contains(joined, "-noaccurate_seek") {
		t.Fatalf("copy-mode seek with audio transcode should use -noaccurate_seek: %s", joined)
	}
	// Should use fMP4 segments.
	if !strings.Contains(joined, "-hls_segment_type fmp4") {
		t.Fatalf("copy-mode should use fMP4 segments: %s", joined)
	}
	// Should have start_number for seek alignment.
	if !strings.Contains(joined, "-start_number 120") {
		t.Fatalf("copy-mode seek should set start_number: %s", joined)
	}
}

func TestBuildFFmpegArgs_MPEG2CopyVideoUsesMPEGTS(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:        "/media/movie.mkv",
		OutputDir:        "/tmp/out",
		SessionID:        "session-mpeg2-copy",
		SourceVideoCodec: "mpeg2video",
		TargetCodecVideo: "copy",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c:v copy") {
		t.Fatalf("mpeg2 copy-mode should preserve video copy: %s", joined)
	}
	if !strings.Contains(joined, "-hls_segment_type mpegts") {
		t.Fatalf("mpeg2 copy-mode should use MPEG-TS HLS segments: %s", joined)
	}
	if !strings.Contains(joined, "seg_%05d.ts") {
		t.Fatalf("mpeg2 copy-mode should write .ts segments: %s", joined)
	}
	if strings.Contains(joined, "movflags=+frag_discont") {
		t.Fatalf("mpeg2 MPEG-TS copy-mode should not use fMP4 movflags: %s", joined)
	}
	for _, enc := range []string{"h264_qsv", "h264_vaapi", "h264_nvenc", "libx264", "hevc_qsv", "hevc_nvenc", "libx265"} {
		if strings.Contains(joined, enc) {
			t.Fatalf("mpeg2 copy-mode should not use encoder %s: %s", enc, joined)
		}
	}
}

func TestBuildFFmpegArgs_MPEG4Part2DisablesHardwareDecode(t *testing.T) {
	for _, hwAccel := range []string{"qsv", "vaapi"} {
		t.Run(hwAccel, func(t *testing.T) {
			args := buildFFmpegArgs(TranscodeOpts{
				InputPath:         "/media/xvid.avi",
				OutputDir:         "/tmp/out",
				SessionID:         "session-xvid",
				SourceVideoCodec:  "mpeg4",
				TargetCodecVideo:  "h264",
				TargetCodecAudio:  "aac",
				SegmentDuration:   2,
				HWAccel:           hwAccel,
				TargetResolution:  "420p",
				TargetBitrateKbps: 720,
			})

			joined := strings.Join(args, " ")
			for _, forbidden := range []string{
				"-hwaccel vaapi",
				"h264_qsv",
				"h264_vaapi",
				"scale_vaapi",
				"hwmap=derive_device=qsv",
			} {
				if strings.Contains(joined, forbidden) {
					t.Fatalf("mpeg4 part 2 source should use software transcode, found %q: %s", forbidden, joined)
				}
			}
			if !strings.Contains(joined, "-c:v libx264") {
				t.Fatalf("mpeg4 part 2 source should fall back to libx264: %s", joined)
			}
			if !strings.Contains(joined, "-vf scale=-2:420") {
				t.Fatalf("mpeg4 part 2 software fallback should preserve requested scaling: %s", joined)
			}
		})
	}
}

func TestRequiresSoftwareVideoDecodeForH264High10(t *testing.T) {
	tests := []struct {
		codec    string
		profile  string
		bitDepth int
		want     bool
	}{
		{codec: "h264", profile: "High 10", bitDepth: 10, want: true},
		{codec: "avc", profile: "Hi10P", bitDepth: 0, want: true},
		{codec: "h264", profile: "High", bitDepth: 10, want: true},
		{codec: "h264", profile: "High", bitDepth: 8, want: false},
		{codec: "hevc", profile: "Main 10", bitDepth: 10, want: false},
	}
	for _, test := range tests {
		if got := RequiresSoftwareVideoDecode(test.codec, test.profile, test.bitDepth); got != test.want {
			t.Errorf("RequiresSoftwareVideoDecode(%q, %q, %d) = %v, want %v", test.codec, test.profile, test.bitDepth, got, test.want)
		}
	}
}

func TestBuildFFmpegArgs_H264High10QSVUsesSoftwareDecodeUpload(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:           "/media/high10.mkv",
		OutputDir:           "/tmp/out",
		SessionID:           "session-high10-pgs-sidecar",
		SourceVideoCodec:    "h264",
		SoftwareVideoDecode: true,
		TargetCodecVideo:    "h264",
		TargetCodecAudio:    "aac",
		SegmentDuration:     2,
		HWAccel:             "qsv",
		TargetResolution:    "720p",
		SubtitleTrackIndex:  2,
		SubtitleCodec:       "hdmv_pgs_subtitle",
	})

	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"-hwaccel vaapi", "-hwaccel_output_format vaapi", "hwdownload"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("High 10 AVC must software-decode, found %q: %s", forbidden, joined)
		}
	}
	for _, required := range []string{
		"-init_hw_device vaapi=va:",
		"-init_hw_device qsv=qs@va",
		"-c:v h264_qsv",
		"-vf scale=-2:720,format=nv12,hwupload,hwmap=derive_device=qsv,format=qsv",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("High 10 QSV recipe missing %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "scale_vaapi") {
		t.Fatalf("High 10 sidecar route must scale software frames before upload: %s", joined)
	}
}

func TestBuildFFmpegArgs_QSVPromotesForcedSegmentKeyframesToIDR(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:        "/media/movie.mkv",
		OutputDir:        "/tmp/out",
		SessionID:        "session-qsv-idr",
		SourceVideoCodec: "vp9",
		TargetCodecVideo: "h264",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
		HWAccel:          "qsv",
	})

	joined := strings.Join(args, " ")
	for _, required := range []string{
		"-force_key_frames expr:gte(t,n_forced*2)",
		"-g 60 -keyint_min 60",
		"-forced_idr 1",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("QSV segment boundary args missing %q: %s", required, joined)
		}
	}
}

func TestBuildFFmpegArgs_NonQSVDoesNotUseQSVForcedIDROption(t *testing.T) {
	for _, hwAccel := range []string{"vaapi", "nvenc", "none"} {
		args := buildFFmpegArgs(TranscodeOpts{
			InputPath:        "/media/movie.mkv",
			OutputDir:        "/tmp/out",
			SessionID:        "session-non-qsv-idr",
			SourceVideoCodec: "vp9",
			TargetCodecVideo: "h264",
			TargetCodecAudio: "aac",
			SegmentDuration:  2,
			HWAccel:          hwAccel,
		})
		if joined := strings.Join(args, " "); strings.Contains(joined, "-forced_idr") {
			t.Fatalf("%s args must not contain QSV-only -forced_idr: %s", hwAccel, joined)
		}
	}
}

func TestBuildFFmpegArgs_H264High10DerivesSoftwareDecodeFromSourceFacts(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:           "/media/high10.mkv",
		OutputDir:           "/tmp/out",
		SessionID:           "session-high10-derived",
		SourceVideoCodec:    "h264",
		SourceVideoProfile:  "High 10",
		SourceVideoBitDepth: 10,
		TargetCodecVideo:    "h264",
		TargetCodecAudio:    "aac",
		SegmentDuration:     2,
		HWAccel:             "qsv",
		TargetResolution:    "720p",
	})

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-hwaccel vaapi") || strings.Contains(joined, "-hwaccel_output_format vaapi") {
		t.Fatalf("High 10 source facts must suppress hardware decode args: %s", joined)
	}
	if !strings.Contains(joined, "-c:v h264_qsv") || !strings.Contains(joined, "format=nv12,hwupload") {
		t.Fatalf("High 10 source facts must retain the software-decode QSV upload path: %s", joined)
	}
}

func TestBuildFFmpegArgs_H264High10QSVASSBurnInUsesSoftwareFrames(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:           "/media/high10.mkv",
		OutputDir:           "/tmp/out",
		SessionID:           "session-high10-ass",
		SourceVideoCodec:    "h264",
		SoftwareVideoDecode: true,
		TargetCodecVideo:    "h264",
		TargetCodecAudio:    "aac",
		SegmentDuration:     2,
		HWAccel:             "qsv",
		TargetResolution:    "720p",
		SubtitleTrackIndex:  0,
		SubtitleBurnIn:      true,
		SubtitleCodec:       "ass",
	})

	joined := strings.Join(args, " ")
	want := "-vf format=yuv420p,scale=-2:720,subtitles='/media/high10.mkv':si=0,format=nv12,hwupload,hwmap=derive_device=qsv,format=qsv"
	if !strings.Contains(joined, want) {
		t.Fatalf("High 10 ASS burn-in should render on software frames then upload %q: %s", want, joined)
	}
	if strings.Contains(joined, "hwdownload") || strings.Contains(joined, "-hwaccel vaapi") {
		t.Fatalf("High 10 ASS burn-in must not assume hardware-decoded input: %s", joined)
	}
}

func TestBuildFFmpegArgs_H264High10QSVBitmapBurnInUsesSoftwareFrames(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:           "/media/high10.mkv",
		OutputDir:           "/tmp/out",
		SessionID:           "session-high10-pgs-burn",
		SourceVideoCodec:    "h264",
		SoftwareVideoDecode: true,
		TargetCodecVideo:    "h264",
		TargetCodecAudio:    "aac",
		SegmentDuration:     2,
		HWAccel:             "qsv",
		TargetResolution:    "720p",
		SubtitleTrackIndex:  2,
		SubtitleBurnIn:      true,
		SubtitleCodec:       "hdmv_pgs_subtitle",
	})

	joined := strings.Join(args, " ")
	want := "-filter_complex [0:v:0]format=yuv420p[vmain];[vmain][0:s:2]overlay=eof_action=pass,scale=-2:720,format=nv12,hwupload,hwmap=derive_device=qsv,format=qsv[vout]"
	if !strings.Contains(joined, want) {
		t.Fatalf("High 10 PGS burn-in should composite on software frames then upload %q: %s", want, joined)
	}
	if strings.Contains(joined, "overlay_vaapi") || strings.Contains(joined, "hwdownload") || strings.Contains(joined, "-hwaccel vaapi") {
		t.Fatalf("High 10 PGS burn-in must not assume hardware-decoded input: %s", joined)
	}
}

func TestBuildFFmpegArgs_BitmapBurnInCPUUsesOverlayFilterComplex(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          "/tmp/out",
		SessionID:          "session-pgs",
		SourceVideoCodec:   "h264",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		HWAccel:            "none",
		TargetResolution:   "1080p",
		SubtitleTrackIndex: 2,
		SubtitleBurnIn:     true,
		SubtitleCodec:      "hdmv_pgs_subtitle",
	})

	joined := strings.Join(args, " ")
	// Overlay runs at native resolution first, then scales.
	want := "-filter_complex [0:v:0][0:s:2]overlay=eof_action=pass,scale=-2:1080[vout]"
	if !strings.Contains(joined, want) {
		t.Fatalf("bitmap burn-in should use overlay filter_complex %q: %s", want, joined)
	}
	// The graph output replaces the raw video stream mapping.
	if !strings.Contains(joined, "-map [vout]") {
		t.Fatalf("bitmap burn-in should map the filter graph output: %s", joined)
	}
	if strings.Contains(joined, "-map 0:v:0") {
		t.Fatalf("bitmap burn-in must not also map the raw video stream: %s", joined)
	}
	// -vf and -filter_complex on the same video stream is an ffmpeg error.
	if strings.Contains(joined, "-vf ") {
		t.Fatalf("bitmap burn-in must not emit -vf alongside -filter_complex: %s", joined)
	}
	if strings.Contains(joined, "subtitles=") {
		t.Fatalf("bitmap burn-in must not use the libass subtitles filter: %s", joined)
	}
	if !strings.Contains(joined, "-c:v libx264") {
		t.Fatalf("bitmap burn-in requires a video encode: %s", joined)
	}
}

func TestBuildFFmpegArgs_BitmapBurnInNoScaleKeepsNativeResolution(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          "/tmp/out",
		SessionID:          "session-pgs-native",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		HWAccel:            "none",
		SubtitleTrackIndex: 0,
		SubtitleBurnIn:     true,
		SubtitleCodec:      "dvd_subtitle",
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-filter_complex [0:v:0][0:s:0]overlay=eof_action=pass[vout]") {
		t.Fatalf("native-resolution bitmap burn-in should overlay without scaling: %s", joined)
	}
}

func TestBuildFFmpegArgsPinsPublishedH264ProfileAndLevelAcrossEncoders(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hw         string
		resolution string
		level      string
	}{
		{name: "software_1080p", hw: "none", resolution: "1080p", level: "4.0"},
		{name: "qsv_1080p", hw: "qsv", resolution: "1080p", level: "4.0"},
		{name: "vaapi_2160p", hw: "vaapi", resolution: "2160p", level: "5.1"},
		{name: "nvenc_2160p", hw: "nvenc", resolution: "2160p", level: "5.1"},
		{name: "original_exact_1440p", hw: "none", resolution: "original", level: "5.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := buildFFmpegArgs(TranscodeOpts{
				InputPath: "/media/movie.mkv", OutputDir: "/tmp/out", SessionID: "session-profile",
				TargetCodecVideo: "h264", TargetCodecAudio: "aac", TargetResolution: tc.resolution,
				TargetVideoWidth:     map[bool]int{true: 2560}[tc.name == "original_exact_1440p"],
				TargetVideoHeight:    map[bool]int{true: 1440}[tc.name == "original_exact_1440p"],
				TargetVideoFrameRate: 30, TargetBitrateKbps: 10_000,
				SegmentDuration: 2, HWAccel: tc.hw,
			})
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "-profile:v high -level:v "+tc.level) {
				t.Fatalf("H.264 output profile/level not pinned: %s", joined)
			}
			if !strings.Contains(joined, "-fpsmax 30") {
				t.Fatalf("H.264 output frame rate is not bounded for the published level: %s", joined)
			}
			if tc.name == "original_exact_1440p" && !strings.Contains(joined, "-vf scale=w='if(eq(gt(iw,ih),gt(2560,1440)),2560,1440)':h='if(eq(gt(iw,ih),gt(2560,1440)),1440,2560)'") {
				t.Fatalf("exact target dimensions were not applied to the video filter: %s", joined)
			}
		})
	}
	if got := h264TranscodeLevelForBoundsV3(1280, 720, 24, 4_096); got != 31 {
		t.Fatalf("bounded 720p24 level = %d, want 31", got)
	}
	if got := h264TranscodeLevelForBoundsV3(176, 144, 15, 100); got != 9 {
		t.Fatalf("Level 1b recipe = %d, want 9", got)
	}
	if got := h264TranscodeLevelForBoundsV3(1920, 1080, 30, 50_000); got != 50 {
		t.Fatalf("high-bitrate 1080p30 level = %d, want 50 because of CPB", got)
	}
	if got := h264TranscodeLevelForBoundsV3(3840, 2160, 30, 50_000); got != 51 {
		t.Fatalf("2160p30 level = %d, want 51", got)
	}
	if got := h264TranscodeLevelForBoundsV3(4096, 2160, 30, 50_000); got != 52 {
		t.Fatalf("DCI 4K 30 level = %d, want 52", got)
	}
	dci24 := buildFFmpegArgs(TranscodeOpts{
		InputPath: "/media/movie.mkv", OutputDir: "/tmp/out", SessionID: "session-dci24",
		TargetCodecVideo: "h264", TargetCodecAudio: "aac", TargetResolution: "2160p",
		TargetVideoWidth: 4096, TargetVideoHeight: 2160, TargetVideoFrameRate: 24,
		TargetBitrateKbps: 20_000,
		SegmentDuration:   2, HWAccel: "none",
	})
	if joined := strings.Join(dci24, " "); !strings.Contains(joined, "-fpsmax 24 -profile:v high -level:v 5.1") {
		t.Fatalf("DCI 4K 24 encoder cadence/profile/level do not match the recipe: %s", joined)
	}
	legacy := buildFFmpegArgs(TranscodeOpts{
		InputPath: "/media/movie.mkv", OutputDir: "/tmp/out", SessionID: "session-legacy",
		TargetCodecVideo: "h264", TargetCodecAudio: "aac", SegmentDuration: 2, HWAccel: "none",
	})
	if joined := strings.Join(legacy, " "); strings.Contains(joined, "-level:v") || strings.Contains(joined, "-fpsmax") {
		t.Fatalf("unbounded legacy transcode must not claim a fabricated H.264 level or cap cadence: %s", joined)
	}
	if got := h264TranscodeLevelForBoundsV3(7680, 4320, 30, 50_000); got != 0 {
		t.Fatalf("unsupported 8K level = %d, want 0", got)
	}
	if got := h264TranscodeLevelForBoundsV3(10_000, 16, 1, 1); got != 0 {
		t.Fatalf("unsupported extreme-aspect level = %d, want 0", got)
	}

	// 8K60 exceeds both the frame-size and bitrate ceilings. Dimensions and
	// bitrate come down; the cadence is legal at 2160p through level 5.2, so
	// it survives untouched.
	plan := PlanV3{EffectiveRecipe: EffectiveRecipeV3{
		VideoCodec: "h264", Width: intPointerV3(7680), Height: intPointerV3(4320),
		FrameRate: floatPointerV3(60), BitrateKbps: intPointerV3(500_000),
	}}
	quality := QualityResultV3{Width: 7680, Height: 4320, BitrateKbps: 500_000, PreservesSource: true}
	if !constrainH264RecipeToSupportedLevelV3(&plan, &quality) {
		t.Fatal("unsupported H.264 recipe was not bounded")
	}
	if got := h264TranscodeLevelForBoundsV3(
		*plan.EffectiveRecipe.Width, *plan.EffectiveRecipe.Height,
		*plan.EffectiveRecipe.FrameRate, *plan.EffectiveRecipe.BitrateKbps,
	); got != 52 {
		t.Fatalf("bounded 8K recipe level = %d, want 52", got)
	}
	if *plan.EffectiveRecipe.Width != 3840 || *plan.EffectiveRecipe.Height != 2160 ||
		*plan.EffectiveRecipe.FrameRate != 60 || *plan.EffectiveRecipe.BitrateKbps != 150_000 {
		t.Fatalf("bounded 8K recipe = %#v", plan.EffectiveRecipe)
	}
}

// A recipe that every H.264 level can already express must be left alone. The
// planner runs this on every transcode, so a blanket cadence cap here would
// silently deliver all 48/50/60 fps content at half its frame rate.
func TestConstrainH264RecipeKeepsLegalHighFrameRateCadence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
		frameRate     float64
		bitrateKbps   int
	}{
		{name: "1080p60", width: 1920, height: 1080, frameRate: 60, bitrateKbps: 8_000},
		{name: "1080p59.94", width: 1920, height: 1080, frameRate: 59.94, bitrateKbps: 8_000},
		{name: "720p120", width: 1280, height: 720, frameRate: 120, bitrateKbps: 6_000},
		{name: "2160p50", width: 3840, height: 2160, frameRate: 50, bitrateKbps: 40_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := PlanV3{EffectiveRecipe: EffectiveRecipeV3{
				VideoCodec: "h264", Width: intPointerV3(tc.width), Height: intPointerV3(tc.height),
				FrameRate: floatPointerV3(tc.frameRate), BitrateKbps: intPointerV3(tc.bitrateKbps),
			}}
			quality := QualityResultV3{Width: tc.width, Height: tc.height, BitrateKbps: tc.bitrateKbps, PreservesSource: true}
			if constrainH264RecipeToSupportedLevelV3(&plan, &quality) {
				t.Fatalf("legal recipe was constrained: %#v", plan.EffectiveRecipe)
			}
			if *plan.EffectiveRecipe.FrameRate != tc.frameRate {
				t.Fatalf("cadence = %v, want %v", *plan.EffectiveRecipe.FrameRate, tc.frameRate)
			}
			if !quality.PreservesSource || quality.RequiresTranscode {
				t.Fatalf("untouched recipe changed the quality decision: %#v", quality)
			}
			if got := h264TranscodeLevelForBoundsV3(tc.width, tc.height, tc.frameRate, tc.bitrateKbps); got == 0 {
				t.Fatal("test case is not a level-expressible recipe")
			}
		})
	}
}

// Cadence is still the last resort when no level can carry the recipe, and it
// halves so the output keeps a clean relationship to the source.
func TestConstrainH264RecipeHalvesCadenceOnlyWhenNoLevelFits(t *testing.T) {
	plan := PlanV3{EffectiveRecipe: EffectiveRecipeV3{
		VideoCodec: "h264", Width: intPointerV3(4096), Height: intPointerV3(2160),
		FrameRate: floatPointerV3(120), BitrateKbps: intPointerV3(45_000),
	}}
	quality := QualityResultV3{Width: 4096, Height: 2160, BitrateKbps: 45_000, PreservesSource: true}
	if !constrainH264RecipeToSupportedLevelV3(&plan, &quality) {
		t.Fatal("DCI 4K120 has no expressible level and was not constrained")
	}
	if *plan.EffectiveRecipe.Width != 4096 || *plan.EffectiveRecipe.Height != 2160 ||
		*plan.EffectiveRecipe.FrameRate != 60 || *plan.EffectiveRecipe.BitrateKbps != 45_000 {
		t.Fatalf("bounded DCI 4K120 recipe = %#v", plan.EffectiveRecipe)
	}
	if got := h264TranscodeLevelForBoundsV3(
		*plan.EffectiveRecipe.Width, *plan.EffectiveRecipe.Height,
		*plan.EffectiveRecipe.FrameRate, *plan.EffectiveRecipe.BitrateKbps,
	); got != 52 {
		t.Fatalf("bounded DCI 4K120 level = %d, want 52", got)
	}
}

func TestBuildFFmpegArgs_BitmapBurnInVAAPICompositesOnGPU(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          "/tmp/out",
		SessionID:          "session-pgs-vaapi",
		SourceVideoCodec:   "h264",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		HWAccel:            "vaapi",
		TargetResolution:   "720p",
		SubtitleTrackIndex: 1,
		SubtitleBurnIn:     true,
		SubtitleCodec:      "hdmv_pgs_subtitle",
	})

	joined := strings.Join(args, " ")
	// Only the subtitle bitmap is uploaded; the video stays on the VAAPI surface
	// and is composited with overlay_vaapi — no full-frame hwdownload roundtrip.
	want := "-filter_complex [0:s:1]format=bgra,hwupload[sub];[0:v:0][sub]overlay_vaapi=eof_action=pass,scale_vaapi=w=-2:h=720:format=nv12[vout]"
	if !strings.Contains(joined, want) {
		t.Fatalf("vaapi bitmap burn-in should composite on GPU %q: %s", want, joined)
	}
	if strings.Contains(joined, "hwdownload") {
		t.Fatalf("vaapi bitmap burn-in must not roundtrip the video through CPU: %s", joined)
	}
	if !strings.Contains(joined, "-map [vout]") {
		t.Fatalf("vaapi bitmap burn-in should map the filter graph output: %s", joined)
	}
	if strings.Contains(joined, "-vf ") {
		t.Fatalf("vaapi bitmap burn-in must not emit -vf: %s", joined)
	}
	if !strings.Contains(joined, "-c:v h264_vaapi") {
		t.Fatalf("vaapi bitmap burn-in should keep the hardware encoder: %s", joined)
	}
}

func TestBuildFFmpegArgs_BitmapBurnInQSVCompositesOnGPU(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          "/tmp/out",
		SessionID:          "session-pgs-qsv",
		SourceVideoCodec:   "h264",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		HWAccel:            "qsv",
		TargetResolution:   "720p",
		SubtitleTrackIndex: 1,
		SubtitleBurnIn:     true,
		SubtitleCodec:      "hdmv_pgs_subtitle",
	})

	joined := strings.Join(args, " ")
	// GPU composite via overlay_vaapi, then map the VAAPI surface to QSV for the
	// encoder — the video never leaves hardware memory.
	want := "-filter_complex [0:s:1]format=bgra,hwupload[sub];[0:v:0][sub]overlay_vaapi=eof_action=pass,scale_vaapi=w=-2:h=720:format=nv12,hwmap=derive_device=qsv,format=qsv[vout]"
	if !strings.Contains(joined, want) {
		t.Fatalf("qsv bitmap burn-in should composite on GPU %q: %s", want, joined)
	}
	if strings.Contains(joined, "hwdownload") {
		t.Fatalf("qsv bitmap burn-in must not roundtrip the video through CPU: %s", joined)
	}
	if strings.Contains(joined, "-vf ") {
		t.Fatalf("qsv bitmap burn-in must not emit -vf: %s", joined)
	}
	if !strings.Contains(joined, "-c:v h264_qsv") {
		t.Fatalf("qsv bitmap burn-in should keep the hardware encoder: %s", joined)
	}
}

func TestBuildFFmpegArgs_BitmapBurnInNVENCStaysOnCPUOverlay(t *testing.T) {
	// overlay_cuda is unverified on the bundled ffmpeg, so NVENC keeps the safe
	// software roundtrip: download the frame, overlay on CPU, re-upload to CUDA.
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          "/tmp/out",
		SessionID:          "session-pgs-nvenc",
		SourceVideoCodec:   "h264",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		HWAccel:            "nvenc",
		TargetResolution:   "720p",
		SubtitleTrackIndex: 1,
		SubtitleBurnIn:     true,
		SubtitleCodec:      "hdmv_pgs_subtitle",
	})

	joined := strings.Join(args, " ")
	want := "-filter_complex [0:v:0]hwdownload,format=yuv420p[vmain];[vmain][0:s:1]overlay=eof_action=pass,scale=-2:720,format=nv12,hwupload_cuda[vout]"
	if !strings.Contains(joined, want) {
		t.Fatalf("nvenc bitmap burn-in should keep the CPU roundtrip %q: %s", want, joined)
	}
	if strings.Contains(joined, "overlay_vaapi") {
		t.Fatalf("nvenc bitmap burn-in must not use the VAAPI GPU overlay: %s", joined)
	}
}

func TestBuildFFmpegArgs_TextBurnInStillUsesSubtitlesFilter(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          "/tmp/out",
		SessionID:          "session-srt",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		HWAccel:            "none",
		TargetResolution:   "1080p",
		SubtitleTrackIndex: 1,
		SubtitleBurnIn:     true,
		SubtitleCodec:      "subrip",
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-vf scale=-2:1080,subtitles='/media/movie.mkv':si=1") {
		t.Fatalf("text burn-in should keep the libass subtitles -vf path: %s", joined)
	}
	if strings.Contains(joined, "-filter_complex") {
		t.Fatalf("text burn-in must not switch to filter_complex: %s", joined)
	}
	if !strings.Contains(joined, "-map 0:v:0") {
		t.Fatalf("text burn-in should keep the raw video stream mapping: %s", joined)
	}
}

func TestBuildFFmpegArgs_LegacyBurnInWithoutCodecKeepsTextPath(t *testing.T) {
	// Recipe cards / tokens minted before SubtitleCodec existed decode with an
	// empty codec; they must reconstruct the exact same (text) command line.
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          "/tmp/out",
		SessionID:          "session-legacy",
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		HWAccel:            "none",
		SubtitleTrackIndex: 0,
		SubtitleBurnIn:     true,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "subtitles='/media/movie.mkv':si=0") {
		t.Fatalf("legacy burn-in without codec should keep the subtitles filter: %s", joined)
	}
	if strings.Contains(joined, "-filter_complex") {
		t.Fatalf("legacy burn-in without codec must not use filter_complex: %s", joined)
	}
}

func TestBuildFFmpegArgs_BitmapBurnInWithCopyVideoIsInert(t *testing.T) {
	// The API layer forces an encode before starting a burn-in transcode; if a
	// copy recipe slips through anyway the builder must stay a valid copy
	// command (no filter graph, raw stream mapping) rather than emit filters
	// against an unencoded stream.
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:          "/media/movie.mkv",
		OutputDir:          "/tmp/out",
		SessionID:          "session-copy-burnin",
		TargetCodecVideo:   "copy",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		SubtitleTrackIndex: 0,
		SubtitleBurnIn:     true,
		SubtitleCodec:      "hdmv_pgs_subtitle",
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c:v copy") {
		t.Fatalf("copy recipe should stay codec copy: %s", joined)
	}
	// Note: "-filter_complex_threads" is a legitimate copy-mode arg; only the
	// filter graph option itself must be absent.
	if strings.Contains(joined, "-filter_complex ") || strings.Contains(joined, "overlay") {
		t.Fatalf("copy recipe must not emit a filter graph: %s", joined)
	}
	if !strings.Contains(joined, "-map 0:v:0") {
		t.Fatalf("copy recipe should map the raw video stream: %s", joined)
	}
}

func TestResolveEffectiveTranscodeHWAccel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts TranscodeOpts
		want string
	}{
		{
			name: "hardware video transcode",
			opts: TranscodeOpts{HWAccel: "qsv", SourceVideoCodec: "h264", TargetCodecVideo: "h264"},
			want: "qsv",
		},
		{
			name: "copy video does not use hardware encode",
			opts: TranscodeOpts{HWAccel: "qsv", SourceVideoCodec: "h264", TargetCodecVideo: "copy"},
			want: "none",
		},
		{
			name: "mpeg4 part 2 falls back to software",
			opts: TranscodeOpts{HWAccel: "vaapi", SourceVideoCodec: "mpeg4", TargetCodecVideo: "h264"},
			want: "none",
		},
		{
			name: "nvenc passthrough",
			opts: TranscodeOpts{HWAccel: "nvenc", SourceVideoCodec: "h264", TargetCodecVideo: "h264"},
			want: "nvenc",
		},
		{
			name: "qsv keeps hardware encode with software decode",
			opts: TranscodeOpts{HWAccel: "qsv", SourceVideoCodec: "h264", SoftwareVideoDecode: true, TargetCodecVideo: "h264"},
			want: "qsv",
		},
		{
			name: "nvenc keeps hardware encode with software decode",
			opts: TranscodeOpts{HWAccel: "nvenc", SourceVideoCodec: "h264", SoftwareVideoDecode: true, TargetCodecVideo: "h264"},
			want: "nvenc",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveEffectiveTranscodeHWAccel(tt.opts); got != tt.want {
				t.Fatalf("resolveEffectiveTranscodeHWAccel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildFFmpegArgs_H264High10NVENCUsesSoftwareDecodeUpload(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:           "/media/high10.mkv",
		OutputDir:           "/tmp/out",
		SessionID:           "session-high10-nvenc",
		SourceVideoCodec:    "h264",
		SourceVideoProfile:  "High 10",
		SourceVideoBitDepth: 10,
		SoftwareVideoDecode: true,
		HWDevice:            "1",
		TargetCodecVideo:    "h264",
		TargetCodecAudio:    "aac",
		SegmentDuration:     2,
		HWAccel:             "nvenc",
		TargetResolution:    "720p",
	})

	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"-hwaccel cuda", "-hwaccel_output_format cuda", "hwdownload"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("High 10 AVC must software-decode before NVENC, found %q: %s", forbidden, joined)
		}
	}
	for _, required := range []string{
		"-init_hw_device cuda=cuda:1 -filter_hw_device cuda",
		"-c:v h264_nvenc",
		"-vf format=nv12,hwupload_cuda,scale_cuda=w=-2:h=720:format=nv12",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("High 10 NVENC recipe missing %q: %s", required, joined)
		}
	}
}

func TestBuildFFmpegArgs_NVENCH264UsesCudaPipeline(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:         "/media/movie.mkv",
		OutputDir:         "/tmp/out",
		SessionID:         "session-nvenc",
		SourceVideoCodec:  "h264",
		TargetCodecVideo:  "h264",
		TargetCodecAudio:  "aac",
		SegmentDuration:   2,
		HWAccel:           "nvenc",
		TargetResolution:  "720p",
		TargetBitrateKbps: 2000,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-hwaccel cuda") {
		t.Fatalf("nvenc args should enable cuda hwaccel: %s", joined)
	}
	if !strings.Contains(joined, "-c:v h264_nvenc") {
		t.Fatalf("nvenc args should use h264_nvenc encoder: %s", joined)
	}
	if !strings.Contains(joined, "-vf scale_cuda=w=-2:h=720:format=nv12") {
		t.Fatalf("nvenc args should use scale_cuda, not software scale: %s", joined)
	}
	if strings.Contains(joined, "-vf scale=-2:720") {
		t.Fatalf("nvenc args must not use software scale on cuda frames: %s", joined)
	}
	if !strings.Contains(joined, "-b:v 2000k -maxrate 2000k -bufsize 4000k") {
		t.Fatalf("nvenc args should include bitrate cap controls: %s", joined)
	}
}

func TestBuildFFmpegArgs_VAAPIScalingUsesHardwareFilter(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:         "/media/movie.mkv",
		OutputDir:         "/tmp/out",
		SessionID:         "session-vaapi",
		SourceVideoCodec:  "h264",
		TargetCodecVideo:  "h264",
		TargetCodecAudio:  "aac",
		SegmentDuration:   2,
		HWAccel:           "vaapi",
		TargetResolution:  "720p",
		TargetBitrateKbps: 2000,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-hwaccel vaapi") {
		t.Fatalf("vaapi args should enable vaapi hwaccel: %s", joined)
	}
	if !strings.Contains(joined, "-vf scale_vaapi=w=-2:h=720:format=nv12") {
		t.Fatalf("vaapi args should use scale_vaapi, not software scale: %s", joined)
	}
	if strings.Contains(joined, "-vf scale=-2:720") {
		t.Fatalf("vaapi args must not use software scale on hardware frames: %s", joined)
	}
}

func TestBuildFFmpegArgs_EncodedTranscodePreservesExistingTimestampPolicy(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath:        "/media/movie.mkv",
		OutputDir:        "/tmp/out",
		SessionID:        "session-encoded",
		SeekSeconds:      2780.63,
		TargetCodecVideo: "h264",
		TargetCodecAudio: "aac",
		SegmentDuration:  2,
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-copyts") {
		t.Fatalf("encoded args should preserve original timestamps: %s", joined)
	}
	if !strings.Contains(joined, "-avoid_negative_ts disabled") {
		t.Fatalf("encoded args should keep avoid_negative_ts disabled: %s", joined)
	}
}

// TranscodesAudio must agree with appendAudioArgs: only an explicit "copy"
// passes audio through; an empty codec runs ffmpeg's AAC default.
func TestTranscodesAudioMatchesFFmpegDefault(t *testing.T) {
	cases := []struct {
		codec string
		want  bool
	}{
		{"copy", false},
		{"COPY", false},
		{"", true},
		{"aac", true},
		{"opus", true},
	}
	for _, tc := range cases {
		if got := TranscodesAudio(tc.codec); got != tc.want {
			t.Errorf("TranscodesAudio(%q) = %v, want %v", tc.codec, got, tc.want)
		}
		args := appendAudioArgs(nil, TranscodeOpts{TargetCodecAudio: tc.codec})
		copied := strings.Contains(strings.Join(args, " "), "-c:a copy")
		if copied != !tc.want {
			t.Errorf("appendAudioArgs(%q) copy=%v disagrees with TranscodesAudio=%v", tc.codec, copied, tc.want)
		}
	}
}

// The published level is derived from the recipe's real cadence, so the encoder
// must be capped at that same cadence rather than at a fixed 30 fps.
func TestBuildFFmpegArgsPublishesTheRecipeCadenceNotAFixedCap(t *testing.T) {
	args := buildFFmpegArgs(TranscodeOpts{
		InputPath: "/media/movie.mkv", OutputDir: "/tmp/out", SessionID: "session-1080p60",
		TargetCodecVideo: "h264", TargetCodecAudio: "aac", TargetResolution: "1080p",
		TargetVideoWidth: 1920, TargetVideoHeight: 1080, TargetVideoFrameRate: 59.94,
		TargetBitrateKbps: 8_000, SegmentDuration: 2, HWAccel: "none",
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-fpsmax 59.94 -profile:v high -level:v 4.2") {
		t.Fatalf("1080p59.94 cadence/profile/level do not match the recipe: %s", joined)
	}
	if strings.Contains(joined, "-fpsmax 30") {
		t.Fatalf("encoder cadence was capped below the planned recipe: %s", joined)
	}
}

// Autorotation must survive the NVENC software-decode path. Its frames stay on
// the CPU until hwupload_cuda, so the transpose ffmpeg inserts is negotiable —
// and neither MPEG-TS nor fMP4 HLS carries a display matrix, so suppressing it
// delivers a rotated source sideways with no way for the client to correct it.
func TestBuildFFmpegArgsKeepsAutorotationForNVENCSoftwareDecode(t *testing.T) {
	base := TranscodeOpts{
		InputPath: "/media/high10.mkv", OutputDir: "/tmp/out", SessionID: "session-nvenc-rotate",
		SourceVideoCodec: "h264", TargetCodecVideo: "h264", TargetCodecAudio: "aac",
		TargetResolution: "1080p", SegmentDuration: 2, HWAccel: "nvenc",
	}

	software := base
	software.SourceVideoProfile = "High 10"
	software.SourceVideoBitDepth = 10
	software.TargetVideoWidth = 1920
	software.TargetVideoHeight = 1080
	softwareArgs := strings.Join(buildFFmpegArgs(software), " ")
	if strings.Contains(softwareArgs, "-noautorotate") {
		t.Fatalf("software-decoded NVENC transcode suppressed autorotation: %s", softwareArgs)
	}
	if !strings.Contains(softwareArgs, "format=nv12,hwupload_cuda") {
		t.Fatalf("expected the NVENC software-decode upload graph: %s", softwareArgs)
	}
	if !strings.Contains(softwareArgs, "scale_cuda=w='if(eq(gt(iw,ih),gt(1920,1080)),1920,1080)':h='if(eq(gt(iw,ih),gt(1920,1080)),1080,1920)':format=nv12") {
		t.Fatalf("NVENC software-decode exact scaling is not rotation-aware: %s", softwareArgs)
	}
	if strings.Contains(softwareArgs, "-hwaccel cuda") {
		t.Fatalf("High 10 must not be handed to the CUDA decoder: %s", softwareArgs)
	}

	// The hardware-decode path still needs it: the transpose cannot be
	// negotiated against CUDA surfaces.
	hardwareArgs := strings.Join(buildFFmpegArgs(base), " ")
	if !strings.Contains(hardwareArgs, "-hwaccel cuda -hwaccel_output_format cuda -noautorotate") {
		t.Fatalf("hardware-decoded NVENC transcode lost autorotation suppression: %s", hardwareArgs)
	}
}

func TestOrientationAwareExactScaleSwapsAxesAfterAutorotation(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}

	filter := "transpose=clock," + targetSoftwareScale(TranscodeOpts{
		TargetVideoWidth:  320,
		TargetVideoHeight: 180,
	}) + ",showinfo"
	cmd := exec.Command(ffmpegPath,
		"-hide_banner", "-loglevel", "info",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=1",
		"-frames:v", "1", "-vf", filter, "-f", "null", "-",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run rotated exact-scale probe: %v\n%s", err, output)
	}
	joined := string(output)
	if !strings.Contains(joined, "s:180x320") || !strings.Contains(joined, "sar:1/1") {
		t.Fatalf("rotated exact-scale output did not preserve display dimensions and square pixels:\n%s", joined)
	}
}
