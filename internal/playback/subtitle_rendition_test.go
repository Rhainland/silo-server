package playback

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

func subtitleRenditionTestOpts(dir string) TranscodeOpts {
	return TranscodeOpts{
		InputPath: "/media/movie.mkv", OutputDir: dir, TargetCodecVideo: "h264", TargetCodecAudio: "aac",
		HWAccel: HWAccelNone, SegmentDuration: 2, TotalDuration: 60,
		SubtitleTrackIndex: 3, SubtitleCodec: "subrip", SubtitleRendition: true,
	}
}

func TestBuildFFmpegArgsAppendsSubtitleRenditionOutput(t *testing.T) {
	dir := t.TempDir()
	opts := subtitleRenditionTestOpts(dir)
	opts.SeekSeconds = 20
	opts.StartSegmentNumber = 10
	opts.subtitleCueFile = subtitleCueFileName(2)
	args := buildFFmpegArgs(opts)

	manifest := slices.Index(args, filepath.Join(dir, "stream.m3u8"))
	if manifest < 0 {
		t.Fatalf("HLS output missing: %v", args)
	}
	// The HLS output itself stays subtitle-free; the cue output follows it so
	// its options cannot leak into the HLS mux.
	if !slices.Contains(args[:manifest], "-sn") {
		t.Fatalf("HLS output must keep -sn: %v", args)
	}
	want := []string{"-y", "-map", "0:s:3", "-c:s", "webvtt", "-flush_packets", "1", "-f", "webvtt", filepath.Join(dir, "subtitles_2.vtt")}
	if got := args[manifest+1:]; !slices.Equal(got, want) {
		t.Fatalf("cue output = %v, want %v", got, want)
	}
	for _, shared := range []string{"-copyts", "-ss"} {
		if !slices.Contains(args[:manifest], shared) {
			t.Fatalf("cue output must share %s with the HLS output: %v", shared, args)
		}
	}
}

func TestSubtitleRenditionInactiveOutsideItsRecipe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*TranscodeOpts)
	}{
		{"not requested", func(o *TranscodeOpts) { o.SubtitleRendition = false }},
		{"no track", func(o *TranscodeOpts) { o.SubtitleTrackIndex = -1 }},
		{"burn in", func(o *TranscodeOpts) { o.SubtitleBurnIn = true }},
		{"bitmap", func(o *TranscodeOpts) { o.SubtitleCodec = "hdmv_pgs_subtitle" }},
		{"copy video", func(o *TranscodeOpts) { o.TargetCodecVideo = "copy" }},
		{"unknown duration", func(o *TranscodeOpts) { o.TotalDuration = 0 }},
		{"oversized playlist", func(o *TranscodeOpts) { o.TotalDuration = float64(maxSyntheticManifestSegments*2 + 10) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := subtitleRenditionTestOpts(t.TempDir())
			tc.mutate(&opts)
			if subtitleRenditionActive(opts) {
				t.Fatal("rendition must be inactive")
			}
			if args := strings.Join(buildFFmpegArgs(opts), " "); strings.Contains(args, "webvtt") {
				t.Fatalf("inactive rendition still emitted a cue output: %s", args)
			}
			session := &TranscodeSession{outputDir: opts.OutputDir, opts: opts}
			if session.HasSubtitleRendition() {
				t.Fatal("inactive rendition published a multivariant playlist")
			}
		})
	}
}

func TestBuildPlaybackManifestPublishesSubtitleRendition(t *testing.T) {
	opts := subtitleRenditionTestOpts(t.TempDir())
	opts.TotalDuration = 9
	opts.TargetBitrateKbps = 4000
	opts.TargetAudioBitrateKbps = 128
	session := &TranscodeSession{outputDir: opts.OutputDir, opts: opts}

	master, err := session.BuildPlaybackManifest("segment/", "st=abc")
	if err != nil {
		t.Fatal(err)
	}
	wantMaster := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-INDEPENDENT-SEGMENTS",
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Subtitles",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI="master.m3u8?silo_rendition=subtitles&st=abc"`,
		`#EXT-X-STREAM-INF:BANDWIDTH=4128000,SUBTITLES="subs"`,
		"master.m3u8?silo_rendition=video&st=abc",
		"",
	}, "\n")
	if string(master) != wantMaster {
		t.Fatalf("multivariant playlist:\n%s\nwant:\n%s", master, wantMaster)
	}

	video, err := session.BuildPlaybackManifest("segment/", "silo_rendition=video&st=abc")
	if err != nil {
		t.Fatal(err)
	}
	if want := session.GenerateFullManifest("segment/", "st=abc"); string(video) != string(want) {
		t.Fatalf("video variant must be the unchanged synthetic playlist:\n%s\nwant:\n%s", video, want)
	}

	subtitles, err := session.BuildPlaybackManifest("segment/", "st=abc&silo_rendition=subtitles")
	if err != nil {
		t.Fatal(err)
	}
	wantSubtitles := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-TARGETDURATION:2",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXTINF:2.000000,", "segment/sub_00000.vtt?st=abc",
		"#EXTINF:2.000000,", "segment/sub_00001.vtt?st=abc",
		"#EXTINF:2.000000,", "segment/sub_00002.vtt?st=abc",
		"#EXTINF:2.000000,", "segment/sub_00003.vtt?st=abc",
		"#EXTINF:1.000000,", "segment/sub_00004.vtt?st=abc",
		"#EXT-X-ENDLIST",
		"",
	}, "\n")
	if string(subtitles) != wantSubtitles {
		t.Fatalf("subtitle playlist:\n%s\nwant:\n%s", subtitles, wantSubtitles)
	}
	videoTimeline, err := parseManifestTimeline(video)
	if err != nil {
		t.Fatal(err)
	}
	if len(videoTimeline.entries) != 5 {
		t.Fatalf("video playlist lists %d segments, subtitle playlist 5", len(videoTimeline.entries))
	}
}

func TestBuildPlaybackManifestRenditionWithoutQuery(t *testing.T) {
	session := &TranscodeSession{opts: subtitleRenditionTestOpts(t.TempDir())}
	master, err := session.BuildPlaybackManifest("segment/", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`URI="master.m3u8?silo_rendition=subtitles"`, "\nmaster.m3u8?silo_rendition=video\n", "BANDWIDTH=8192000"} {
		if !strings.Contains(string(master), want) {
			t.Fatalf("multivariant playlist missing %q:\n%s", want, master)
		}
	}
}

func TestAppendManifestQueryParamCoversSubtitleRendition(t *testing.T) {
	session := &TranscodeSession{opts: subtitleRenditionTestOpts(t.TempDir())}
	master, err := session.BuildPlaybackManifest("segment/", "")
	if err != nil {
		t.Fatal(err)
	}
	got := string(AppendManifestQueryParam(master, "st", "tok"))
	for _, want := range []string{`URI="master.m3u8?silo_rendition=subtitles&st=tok"`, "\nmaster.m3u8?silo_rendition=video&st=tok\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("relay token missing from %q:\n%s", want, got)
		}
	}
}

func TestSubtitleSegmentNamesStayOutOfTheVideoPipeline(t *testing.T) {
	if n, ok := ParseSubtitleSegmentNumber("sub_00012.vtt"); !ok || n != 12 {
		t.Fatalf("ParseSubtitleSegmentNumber = %d, %v", n, ok)
	}
	for _, name := range []string{"seg_00012.ts", "sub_.vtt", "sub_00012.ts", "sub_-1.vtt", "subtitles_0.vtt"} {
		if _, ok := ParseSubtitleSegmentNumber(name); ok {
			t.Fatalf("%q parsed as a subtitle segment", name)
		}
	}
	// Segment acknowledgement, pruning, and restart all key off
	// ParseSegmentNumber; a subtitle segment must never look like media.
	if _, err := ParseSegmentNumber("sub_00012.vtt"); err == nil {
		t.Fatal("subtitle segment parsed as a media segment")
	}
}

const subtitleRenditionCues = `WEBVTT

00:01.000 --> 00:03.500
First cue

00:21.000 --> 00:25.000 align:start position:10%
After the gap,
spans segments

01:00:40.500 --> 01:00:41.500
Late cue
`

func TestSubtitleSegmentDocumentCutsTheFixedGrid(t *testing.T) {
	opts := subtitleRenditionTestOpts("")
	opts.TotalDuration = 2 * 60 * 60
	cues := []byte(subtitleRenditionCues)
	header := "WEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:900000\n"
	spanning := "\n00:21.000 --> 00:25.000 align:start position:10%\nAfter the gap,\nspans segments\n"
	for _, tc := range []struct {
		segment int
		want    string
	}{
		{0, header + "\n00:01.000 --> 00:03.500\nFirst cue\n"},
		{1, header + "\n00:01.000 --> 00:03.500\nFirst cue\n"},
		{2, header},
		{10, header + spanning},
		{11, header + spanning},
		{12, header + spanning},
		{13, header},
		{1820, header + "\n01:00:40.500 --> 01:00:41.500\nLate cue\n"},
	} {
		if got := string(subtitleSegmentDocument(opts, tc.segment, cues)); got != tc.want {
			t.Errorf("segment %d:\n%q\nwant\n%q", tc.segment, got, tc.want)
		}
	}
}

func TestSubtitleSegmentDocumentTimestampMapFollowsTheContainer(t *testing.T) {
	opts := subtitleRenditionTestOpts("")
	opts.TargetCodecVideo = transcodeCodecHEVC
	if got := string(subtitleSegmentDocument(opts, 0, nil)); got != "WEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:0\n" {
		t.Fatalf("fMP4 segments keep the source clock: %q", got)
	}
}

func TestSubtitleSegmentDocumentKeepsCuesPastTheLastBoundary(t *testing.T) {
	opts := subtitleRenditionTestOpts("")
	opts.TotalDuration = 21
	// The last listed segment (10) covers the remainder and anything a
	// slightly longer real stream carries past the probed duration.
	got := string(subtitleSegmentDocument(opts, 10, []byte(subtitleRenditionCues)))
	if !strings.Contains(got, "Late cue") || !strings.Contains(got, "After the gap") {
		t.Fatalf("last segment dropped trailing cues: %q", got)
	}
}

func TestSubtitleSegmentDocumentSkipsACueStillBeingWritten(t *testing.T) {
	opts := subtitleRenditionTestOpts("")
	partial := []byte("WEBVTT\n\n00:01.000 --> 00:03.500\nFirst cue\n\n00:02.000 --> 00:0")
	got := string(subtitleSegmentDocument(opts, 1, partial))
	if strings.Count(got, "-->") != 1 || !strings.Contains(got, "First cue") {
		t.Fatalf("partial cue leaked into the segment: %q", got)
	}
}

func writeSubtitleRenditionPlaylist(t *testing.T, dir string, first, last int) {
	t.Helper()
	if err := writeSubtitleRenditionPlaylistFile(dir, first, last); err != nil {
		t.Fatal(err)
	}
}

// writeSubtitleRenditionPlaylistFile is the goroutine-safe form: it reports
// the error instead of failing the test.
func writeSubtitleRenditionPlaylistFile(dir string, first, last int) error {
	lines := []string{"#EXTM3U", "#EXT-X-VERSION:3", "#EXT-X-TARGETDURATION:2", "#EXT-X-MEDIA-SEQUENCE:" + strconv.Itoa(first)}
	for segment := first; segment <= last; segment++ {
		lines = append(lines, "#EXTINF:2.000000,", fmt.Sprintf("seg_%05d.ts", segment))
	}
	return os.WriteFile(filepath.Join(dir, "stream.m3u8"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func TestSubtitleSegmentVTTServesOnlyDemuxedSegments(t *testing.T) {
	dir := t.TempDir()
	opts := subtitleRenditionTestOpts(dir)
	opts.subtitleCueFile = subtitleCueFileName(0)
	if err := os.WriteFile(filepath.Join(dir, opts.subtitleCueFile), []byte(subtitleRenditionCues), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSubtitleRenditionPlaylist(t, dir, 10, 12)
	opts.StartSegmentNumber = 10
	session := &TranscodeSession{outputDir: dir, opts: opts, running: true}
	session.startSubtitleSpanLocked(opts)

	got, err := session.SubtitleSegmentVTT(t.Context(), 11)
	if err != nil || !strings.Contains(string(got), "After the gap") {
		t.Fatalf("segment behind the produced head: %q, %v", got, err)
	}

	// Segment 12 is the newest the process finished: its next segment is not
	// written, so later cues may still be in flight.
	if _, ready, _, imminent := session.subtitleSegmentSource(12); ready || !imminent {
		t.Fatalf("live-edge segment ready=%v imminent=%v, want a long wait for production", ready, imminent)
	}
	// A segment far past the produced output waits briefly and ends in a
	// valid empty document rather than an error.
	session.opts.SegmentDuration = 1 // shortest bounded wait
	start := time.Now()
	got, err = session.SubtitleSegmentVTT(t.Context(), 40)
	if err != nil || strings.Contains(string(got), "-->") || !strings.HasPrefix(string(got), "WEBVTT\nX-TIMESTAMP-MAP=") {
		t.Fatalf("far segment: %q, %v", got, err)
	}
	if waited := time.Since(start); waited < minSegmentWait || waited > maxSegmentWait+time.Second {
		t.Fatalf("far-segment wait took %v", waited)
	}

	// Before the generation's start, nothing covers the segment.
	session.mu.Lock()
	session.running = false
	session.mu.Unlock()
	start = time.Now()
	got, err = session.SubtitleSegmentVTT(t.Context(), 9)
	if err != nil || strings.Contains(string(got), "-->") || time.Since(start) > time.Second {
		t.Fatalf("uncovered segment of a stopped session must answer at once: %q, %v", got, err)
	}

	// A process that exited cleanly finished every segment it listed.
	got, err = session.SubtitleSegmentVTT(t.Context(), 12)
	if err != nil || !strings.HasPrefix(string(got), "WEBVTT") {
		t.Fatalf("finished segment: %q, %v", got, err)
	}
}

func TestSubtitleSegmentVTTWakesWhenProductionCatchesUp(t *testing.T) {
	dir := t.TempDir()
	opts := subtitleRenditionTestOpts(dir)
	opts.subtitleCueFile = subtitleCueFileName(0)
	if err := os.WriteFile(filepath.Join(dir, opts.subtitleCueFile), []byte(subtitleRenditionCues), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSubtitleRenditionPlaylist(t, dir, 0, 10)
	session := &TranscodeSession{outputDir: dir, opts: opts, running: true}
	session.startSubtitleSpanLocked(opts)

	produced := make(chan error, 1)
	go func() {
		//nolint:forbidigo // Simulates FFmpeg finishing a segment while the request is already waiting.
		time.Sleep(3 * subtitleCuePollInterval)
		produced <- writeSubtitleRenditionPlaylistFile(dir, 0, 12)
	}()
	got, err := session.SubtitleSegmentVTT(t.Context(), 11)
	if writeErr := <-produced; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || !strings.Contains(string(got), "After the gap") {
		t.Fatalf("waited segment: %q, %v", got, err)
	}
}

// After a seek restart on slow storage the new generation can take longer than
// the short wait to reach the segment the player asks for first. An empty
// answer would be final for a VOD player, so a segment the live generation is
// about to produce waits as long as its video segment does.
func TestSubtitleSegmentVTTWaitsOutASlowRestart(t *testing.T) {
	dir := t.TempDir()
	opts := subtitleRenditionTestOpts(dir)
	opts.SegmentDuration = 1 // the short wait is its minimum; cue 00:21 is in segment 21
	opts.StartSegmentNumber = 20
	opts.subtitleCueFile = subtitleCueFileName(0)
	if err := os.WriteFile(filepath.Join(dir, opts.subtitleCueFile), []byte(subtitleRenditionCues), 0o644); err != nil {
		t.Fatal(err)
	}
	session := &TranscodeSession{outputDir: dir, opts: opts, running: true}
	session.startSubtitleSpanLocked(opts)

	produced := make(chan error, 1)
	go func() {
		//nolint:forbidigo // Simulates a restarted FFmpeg reaching the segment after the short wait.
		time.Sleep(minSegmentWait + time.Second)
		produced <- writeSubtitleRenditionPlaylistFile(dir, 20, 22)
	}()
	got, err := session.SubtitleSegmentVTT(t.Context(), 21)
	if writeErr := <-produced; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || !strings.Contains(string(got), "After the gap") {
		t.Fatalf("segment after a slow restart: %q, %v", got, err)
	}
}

func TestSubtitleSegmentVTTIgnoresAnInheritedPlaylist(t *testing.T) {
	dir := t.TempDir()
	opts := subtitleRenditionTestOpts(dir)
	opts.subtitleCueFile = subtitleCueFileName(0)
	if err := os.WriteFile(filepath.Join(dir, opts.subtitleCueFile), []byte(subtitleRenditionCues), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSubtitleRenditionPlaylist(t, dir, 0, 20)
	session := &TranscodeSession{outputDir: dir, opts: opts, inheritedManifest: statManifest(dir)}
	session.startSubtitleSpanLocked(opts)
	got, err := session.SubtitleSegmentVTT(t.Context(), 11)
	if err != nil || strings.Contains(string(got), "-->") {
		t.Fatalf("an earlier generation's playlist is not this process's progress: %q, %v", got, err)
	}
}

func TestSubtitleSpansKeepEarlierGenerationsReadable(t *testing.T) {
	dir := t.TempDir()
	opts := subtitleRenditionTestOpts(dir)
	opts.subtitleCueFile = subtitleCueFileName(0)
	if err := os.WriteFile(filepath.Join(dir, opts.subtitleCueFile), []byte(subtitleRenditionCues), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSubtitleRenditionPlaylist(t, dir, 0, 14)
	session := &TranscodeSession{outputDir: dir, opts: opts}
	session.startSubtitleSpanLocked(opts)

	// A seek restart at segment 900 closes generation 0 at its head.
	session.closeSubtitleSpan()
	next := opts
	next.StartSegmentNumber = 900
	next.subtitleCueFile = subtitleCueFileName(1)
	if err := os.WriteFile(filepath.Join(dir, next.subtitleCueFile), []byte("WEBVTT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSubtitleRenditionPlaylist(t, dir, 900, 901)
	session.mu.Lock()
	session.running = true
	session.opts = next
	session.startSubtitleSpanLocked(next)
	session.mu.Unlock()

	if want := []subtitleCueSpan{{file: "subtitles_0.vtt", start: 0, end: 14}, {file: "subtitles_1.vtt", start: 900, end: -1}}; !slices.Equal(session.subtitleSpans, want) {
		t.Fatalf("spans = %#v, want %#v", session.subtitleSpans, want)
	}
	// A backward seek into generation 0's output still has its cues; the
	// boundary segment it finished last is included.
	got, err := session.SubtitleSegmentVTT(t.Context(), 12)
	if err != nil || !strings.Contains(string(got), "After the gap") {
		t.Fatalf("earlier generation's segment: %q, %v", got, err)
	}
	if file, ready, _, _ := session.subtitleSegmentSource(14); !ready || file != "subtitles_0.vtt" {
		t.Fatalf("closed span must cover its head: %q, %v", file, ready)
	}
	if _, ready, _, _ := session.subtitleSegmentSource(15); ready {
		t.Fatal("segment past the closed span's head must not be served from it")
	}
	if file, ready, _, _ := session.subtitleSegmentSource(900); !ready || file != "subtitles_1.vtt" {
		t.Fatalf("live generation must serve its own segments: %q, %v", file, ready)
	}
}

func TestSplitSubtitleRenditionQueryKeepsOtherParametersVerbatim(t *testing.T) {
	rendition, rest := splitSubtitleRenditionQuery("a=1&silo_rendition=video&st=x%2By&b")
	if rendition != subtitleRenditionVideo || rest != "a=1&st=x%2By&b" {
		t.Fatalf("got %q, %q", rendition, rest)
	}
}

// TestSubtitleRenditionAlignsWithRealFFmpegSegments runs the transcode's own
// command line and checks the contract the player relies on: the cue output
// carries source-clock cues, and the segment X-TIMESTAMP-MAP maps them onto
// the video segments' actual timestamps after a seek restart.
func TestSubtitleRenditionAlignsWithRealFFmpegSegments(t *testing.T) {
	if testing.Short() {
		t.Skip("real FFmpeg integration test")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ffprobe := ffprobePathFromFFmpeg(ffmpeg)
	if _, err := exec.LookPath(ffprobe); err != nil {
		t.Skip("ffprobe is not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	run := func(t *testing.T, binary string, args ...string) []byte {
		t.Helper()
		output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", filepath.Base(binary), err, output)
		}
		return output
	}
	work := t.TempDir()
	srt := filepath.Join(work, "subs.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,000\nEarly\n\n2\n00:00:21,000 --> 00:00:25,000\nAcross segments\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(work, "source.mkv")
	run(t, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-i", srt, "-t", "30",
		"-map", "0", "-map", "1", "-map", "2", "-c:v", "ffv1", "-c:a", "pcm_s16le", "-c:s", "srt", source)

	for _, tc := range []struct {
		name, video string
		offset90k   int64
	}{
		{"mpegts", transcodeCodecH264, HLSMPEGTSTimestampOffset90k},
		{"fmp4", transcodeCodecHEVC, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.video == transcodeCodecHEVC && !strings.Contains(string(run(t, ffmpeg, "-hide_banner", "-encoders")), "libx265") {
				t.Skip("libx265 is unavailable")
			}
			dir := t.TempDir()
			opts := TranscodeOpts{
				InputPath: source, OutputDir: dir, FFmpegPath: ffmpeg, HWAccel: HWAccelNone,
				SourceVideoCodec: "ffv1", TargetCodecVideo: tc.video, TargetCodecAudio: "aac",
				SeekSeconds: 20, StartSegmentNumber: 10, SegmentDuration: 2, TotalDuration: 30,
				SubtitleTrackIndex: 0, SubtitleCodec: "subrip", SubtitleRendition: true,
				subtitleCueFile: subtitleCueFileName(0),
			}
			if tc.video == transcodeCodecHEVC {
				opts.VideoSampleEntry = VideoSampleEntryHVC1
			}
			// A reconstructed session reuses its predecessor's directory and
			// cue file name; ffmpeg must replace the file, not stop to ask.
			if err := os.WriteFile(filepath.Join(dir, opts.subtitleCueFile), []byte("WEBVTT\n\n00:21.000 --> 00:22.000\nStale\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, ffmpeg, buildFFmpegArgs(opts)...)

			session := &TranscodeSession{outputDir: dir, opts: opts}
			session.startSubtitleSpanLocked(opts)
			document, err := session.SubtitleSegmentVTT(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			text := string(document)
			wantMap := fmt.Sprintf("X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:%d\n", tc.offset90k)
			if !strings.Contains(text, wantMap) || !strings.Contains(text, "00:21.000 --> 00:25.000\nAcross segments") {
				t.Fatalf("segment 10:\n%s", text)
			}
			if strings.Contains(text, "Early") || strings.Contains(text, "Stale") {
				t.Fatalf("segment 10 carries a cue from another window:\n%s", text)
			}

			// The first video frame of segment 10 is source time 20 s. Mapped
			// through X-TIMESTAMP-MAP, cue time 20 s must land on its PTS.
			var report struct {
				Packets []struct {
					Type string `json:"codec_type"`
					PTS  string `json:"pts_time"`
				} `json:"packets"`
			}
			output := run(t, ffprobe, "-v", "error", "-show_packets", "-show_entries", "packet=codec_type,pts_time", "-of", "json", filepath.Join(dir, "stream.m3u8"))
			if err := json.Unmarshal(output, &report); err != nil {
				t.Fatal(err)
			}
			firstVideo := math.Inf(1)
			for _, packet := range report.Packets {
				if packet.Type != "video" {
					continue
				}
				if pts, err := strconv.ParseFloat(packet.PTS, 64); err == nil {
					firstVideo = math.Min(firstVideo, pts)
				}
			}
			want := 20 + float64(tc.offset90k)/90_000
			if math.Abs(firstVideo-want) > 0.1 {
				t.Fatalf("first video PTS %.3f, cue clock maps source 20 s to %.3f", firstVideo, want)
			}
		})
	}
}

func subtitleRenditionPlannerInputV3(t *testing.T, codec string, fidelity SubtitleFidelityV3) PlannerInputV3 {
	t.Helper()
	file := detailedFixtureFileV3()
	file.Duration = 6480
	file.VideoTracks[0].VideoRange = "SDR"
	file.VideoTracks[0].VideoRangeType = "SDR"
	file.VideoTracks[0].ColorTransfer = "bt709"
	file.SubtitleTracks = []models.SubtitleTrack{{Index: 3, Codec: codec, Language: "eng"}}
	req := validStartRequestV3()
	req.QualityPreference = "1080p"
	req.SubtitleFidelityPreference = fidelity
	req.Capabilities.VideoDecode = []VideoDecodeCapabilityV3{{Codec: "hevc", Profiles: []string{"main 10"}, Levels: []int{153}, BitDepths: []int{10}, MaxWidth: 3840, MaxHeight: 2160, MaxFrameRate: 60, MaxBitrateKbps: 80_000, Hardware: true}}
	hls := req.ClientPlaybackContext.Deliveries[DeliveryClassHLSV3]
	hls.Subtitles = DeliverySubtitleCapabilitiesV3{SidecarText: true, EmbeddedText: true}
	req.ClientPlaybackContext.Deliveries[DeliveryClassHLSV3] = hls
	index := 0
	req.SubtitleTrackIndex = &index
	req.SubtitleTrackID = TrackIDV3(file.ID, "subtitle", index)
	req.ClientFeatures = append(req.ClientFeatures, FeatureHLSSubtitleRenditionsV3)
	return PlannerInputV3{
		Request: req, RequestedFile: file, EffectiveFile: file, AudioTrackIndex: 0,
		Settings: PlannerSettingsV3{TranscodeEnabled: true, Allow4KTranscode: true}, Registry: testTransformationRegistryV3(),
		SubtitleRenditionExecutable: func() bool { return true },
	}
}

func TestPlanPlaybackV3CarriesEmbeddedTextAsHLSSubtitleRendition(t *testing.T) {
	input := subtitleRenditionPlannerInputV3(t, "subrip", SubtitleFidelityCompatibleV3)
	result := PlanPlaybackV3(input)
	if result.Plan == nil || result.Plan.Delivery != DeliveryTranscodeHLSV3 {
		t.Fatalf("want a transcode, got %s", ExplainPlannerResultV3(result))
	}
	want := &HLSSubtitleRenditionV3{GroupID: "subs", Name: "Subtitles", Format: "vtt", MIMEType: "text/vtt"}
	subtitle := result.Plan.Subtitle
	if !result.SubtitleRendition || subtitle.HLSRendition == nil || *subtitle.HLSRendition != *want ||
		subtitle.Artifact != nil || subtitle.Embedded != nil || subtitle.Mode != SubtitleRenderV3 || subtitle.TrackID != input.Request.SubtitleTrackID {
		t.Fatalf("subtitle decision = %#v (rendition %v)", subtitle, result.SubtitleRendition)
	}
	if result.SubtitleTransportTrackIndex != 0 || result.SubtitleBurnIn || result.Plan.Claims.Subtitles.Reason != "server_hls_subtitle_rendition" {
		t.Fatalf("recipe = %#v", result)
	}
	recipe := FreezeExecutableRecipeV3(result)
	if !recipe.SubtitleRendition || recipe.Version != executableRecipeVersionSubtitleRenditionV3 || !recipe.ValidFor(*result.Plan) ||
		!recipe.PlannerResult(result.Plan).SubtitleRendition {
		t.Fatalf("frozen recipe lost the rendition: %#v", recipe)
	}

	// A failed rendition falls back to the sidecar plan instead of exhausting
	// the route.
	retry := input
	retry.AttemptedKeys = []string{result.Plan.PlanAttemptKey}
	fallback := PlanPlaybackV3(retry)
	if fallback.Plan == nil || fallback.SubtitleRendition || fallback.Plan.Subtitle.HLSRendition != nil ||
		fallback.Plan.Delivery != DeliveryTranscodeHLSV3 || fallback.Plan.PlanAttemptKey == result.Plan.PlanAttemptKey {
		t.Fatalf("rendition retry = %s", ExplainPlannerResultV3(fallback))
	}
}

func TestPlanPlaybackV3KeepsSidecarWithoutHLSSubtitleRendition(t *testing.T) {
	for _, tc := range []struct {
		name     string
		codec    string
		fidelity SubtitleFidelityV3
		mutate   func(*PlannerInputV3)
	}{
		{"client did not negotiate", "subrip", SubtitleFidelityCompatibleV3, func(in *PlannerInputV3) {
			in.Request.ClientFeatures = WithoutFeatureV3(in.Request.ClientFeatures, FeatureHLSSubtitleRenditionsV3)
		}},
		{"no capable executor", "subrip", SubtitleFidelityCompatibleV3, func(in *PlannerInputV3) {
			in.SubtitleRenditionExecutable = func() bool { return false }
		}},
		{"unknown duration", "subrip", SubtitleFidelityCompatibleV3, func(in *PlannerInputV3) { in.EffectiveFile.Duration = 0 }},
		{"ASS keeps its styling", "ass", SubtitleFidelityPreserveV3, func(in *PlannerInputV3) {
			hls := in.Request.ClientPlaybackContext.Deliveries[DeliveryClassHLSV3]
			hls.Subtitles.ASSStyling, hls.Subtitles.FontAttachments = true, true
			in.Request.ClientPlaybackContext.Deliveries[DeliveryClassHLSV3] = hls
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := subtitleRenditionPlannerInputV3(t, tc.codec, tc.fidelity)
			tc.mutate(&input)
			result := PlanPlaybackV3(input)
			if result.Plan == nil || result.Plan.Delivery != DeliveryTranscodeHLSV3 {
				t.Fatalf("want a transcode, got %s", ExplainPlannerResultV3(result))
			}
			if result.SubtitleRendition || result.Plan.Subtitle.HLSRendition != nil {
				t.Fatalf("unexpected rendition: %#v", result.Plan.Subtitle)
			}
		})
	}
}

func TestPlanPlaybackV3ConvertsASSIntoHLSSubtitleRendition(t *testing.T) {
	result := PlanPlaybackV3(subtitleRenditionPlannerInputV3(t, "ass", SubtitleFidelityCompatibleV3))
	if result.Plan == nil || !result.SubtitleRendition || result.Plan.Subtitle.HLSRendition == nil {
		t.Fatalf("ASS that is converted anyway can use the rendition: %s", ExplainPlannerResultV3(result))
	}
}

func TestRecipeCardCarriesSubtitleRendition(t *testing.T) {
	opts := subtitleRenditionTestOpts(t.TempDir())
	card := NewRecipeCard(1, "profile", 42, "", opts)
	claims := card.ToClaims()
	if !card.SubtitleRendition || !claims.SubtitleRendition || !RecipeCardFromClaims(&claims).SubtitleRendition ||
		!card.TranscodeOpts(opts.OutputDir, "", nil).SubtitleRendition {
		t.Fatal("subtitle rendition lost between recipe card, claims, and options")
	}
}
