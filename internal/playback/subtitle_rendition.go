package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// An HLS subtitle rendition delivers the selected embedded text track inside
// the transcode's own presentation instead of as a whole-file sidecar.
//
// The transcode's ffmpeg process writes the track as one continuous WebVTT
// file per generation (a second output beside the HLS mux, cues on the same
// -copyts source clock as the video). The server cuts that file into segments
// on the synthetic playlist's fixed grid: subtitle segment N carries every
// cue overlapping [S+N*d, S+(N+1)*d), where S is the video's first source
// timestamp, and its X-TIMESTAMP-MAP ties cue time zero
// to the video segments' timestamp origin. Demuxing the subtitle as part of
// the transcode means a cue is on disk once the video covering it is, so a
// subtitle segment is never slower than the video segment it accompanies.
//
// The rendition is served through the existing master.m3u8 and segment/
// routes, so every relay (API, proxy token and grant routes, node) carries it
// unchanged: master.m3u8 becomes a multivariant playlist whose variant and
// subtitle playlists are master.m3u8 again, selected by SubtitleRenditionQueryParam.

// SubtitleRenditionQueryParam selects a playlist of a session that publishes
// a subtitle rendition. Absent, master.m3u8 answers the multivariant playlist.
const SubtitleRenditionQueryParam = "silo_rendition"

const (
	subtitleRenditionVideo     = "video"
	subtitleRenditionSubtitles = "subtitles"
	// SubtitleRenditionGroupID is the EXT-X-MEDIA GROUP-ID the variant names
	// in its SUBTITLES attribute.
	SubtitleRenditionGroupID = "subs"
	// SubtitleRenditionName is the rendition's NAME. The plan's inventory
	// already describes the track; the playlist only has to be valid.
	SubtitleRenditionName   = "Subtitles"
	subtitleSegmentPrefix   = "sub_"
	subtitleSegmentExt      = ".vtt"
	subtitleCuePollInterval = 100 * time.Millisecond
	// subtitleSegmentProductionWait bounds the wait for a segment the live
	// generation is about to produce, matching what its video segment gets
	// after a seek restart. An empty answer is final for a VOD player, so a
	// cue produced a moment later would never be shown.
	subtitleSegmentProductionWait = 30 * time.Second
	// The multivariant playlist needs a BANDWIDTH even for an uncapped
	// encode; a single variant only uses it as a startup hint.
	subtitleRenditionDefaultVideoKbps = 8000
	subtitleRenditionDefaultAudioKbps = 192
	// subtitleClockProbeTimeout bounds the header-only probe of the video's
	// start time. It reads no more than FFmpeg's own startup probe does.
	subtitleClockProbeTimeout = 15 * time.Second
)

// SubtitleRenditionCodecSupported reports whether ffmpeg can write a source
// subtitle codec as WebVTT text: every text codec except CEA-608 captions,
// which travel inside the video stream rather than as a subtitle stream.
func SubtitleRenditionCodecSupported(codec string) bool {
	return isTextSubtitleV3(codec) && !strings.Contains(normalizeCodecV3(codec), "608")
}

// SubtitleRenditionExecutable reports whether a transcode with these facts can
// carry a subtitle rendition: an encoded (not copied) video whose playlist is
// the synthetic fixed-grid VOD playlist the subtitle segments align to.
func SubtitleRenditionExecutable(targetVideoCodec string, totalDuration float64, segmentDuration int) bool {
	if segmentDuration <= 0 {
		segmentDuration = defaultSegmentDuration
	}
	return !strings.EqualFold(targetVideoCodec, "copy") && totalDuration > 0 &&
		CanGenerateSyntheticManifest(totalDuration, segmentDuration)
}

// subtitleRenditionActive is the single gate shared by the ffmpeg output, the
// playlists, and segment serving, so they cannot disagree about a recipe.
func subtitleRenditionActive(opts TranscodeOpts) bool {
	return opts.SubtitleRendition && opts.SubtitleTrackIndex >= 0 && !opts.SubtitleBurnIn &&
		SubtitleRenditionCodecSupported(opts.SubtitleCodec) &&
		SubtitleRenditionExecutable(opts.TargetCodecVideo, opts.TotalDuration, opts.SegmentDuration)
}

func subtitleCueFileName(generation uint64) string {
	return fmt.Sprintf("subtitles_%d.vtt", generation)
}

// appendSubtitleRenditionOutputArgs adds the WebVTT cue output after the HLS
// output. -flush_packets writes every cue as soon as it is demuxed, which is
// what lets a subtitle segment be served once the video around it exists.
// -y lets it replace a cue file of the same name: a reconstructed session
// reuses its predecessor's output directory and numbers generations from 0
// again, and without -y ffmpeg would wait at its overwrite prompt on the open
// stdin pipe instead of transcoding.
func appendSubtitleRenditionOutputArgs(args []string, opts TranscodeOpts) []string {
	if !subtitleRenditionActive(opts) {
		return args
	}
	cueFile := opts.subtitleCueFile
	if cueFile == "" {
		cueFile = subtitleCueFileName(0)
	}
	return append(args,
		"-y",
		"-map", fmt.Sprintf("0:s:%d", opts.SubtitleTrackIndex),
		"-c:s", "webvtt",
		"-flush_packets", "1",
		"-f", "webvtt",
		filepath.Join(opts.OutputDir, cueFile),
	)
}

// The cue file keeps the source's absolute clock (-copyts), and the encoded
// video keeps it too: video segment N starts at the video's first source
// timestamp plus N*d, not at N*d. A container that does not start at 0 (MPEG-TS
// sources, remuxed broadcast captures) would otherwise put every cue into a
// later segment than the video it belongs to. The session probes that start
// time once, beside FFmpeg's own startup, and every generation's segment
// windows are cut from it.

// startSubtitleClockProbe probes the video's start time in the background.
// Subtitle segments wait for the answer before they are cut; a failed probe
// answers 0, which keeps cues on a 0-based grid. The probe does not run on
// the process context: a seek restart cancels that, and the answer belongs to
// the source, so it has to outlive the generation that started it. Only the
// session's shutdown cancels it (stopSubtitleClockProbe).
func (s *TranscodeSession) startSubtitleClockProbe(ctx context.Context, opts TranscodeOpts) {
	if !subtitleRenditionActive(opts) {
		return
	}
	probeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ready := make(chan struct{})
	s.mu.Lock()
	s.subtitleClockReady = ready
	s.subtitleClockCancel = cancel
	s.mu.Unlock()
	go func() {
		defer close(ready)
		defer cancel()
		origin, err := probeSubtitleClockOrigin(probeCtx, opts)
		if err != nil {
			if probeCtx.Err() == nil {
				log.Printf("playback: session %s subtitle rendition start-time probe failed, cutting cues from 0: %v", opts.SessionID, err)
			}
			return
		}
		s.mu.Lock()
		s.subtitleClockOriginMS = origin
		s.mu.Unlock()
	}()
}

// stopSubtitleClockProbe cancels a probe still running when the session
// shuts down.
func (s *TranscodeSession) stopSubtitleClockProbe() {
	s.mu.Lock()
	cancel := s.subtitleClockCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// subtitleClockOrigin returns the video's first source timestamp in
// milliseconds, waiting for the probe when it is still running.
func (s *TranscodeSession) subtitleClockOrigin(ctx context.Context) (int64, error) {
	s.mu.Lock()
	ready := s.subtitleClockReady
	s.mu.Unlock()
	if ready != nil {
		select {
		case <-ready:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subtitleClockOriginMS, nil
}

// probeSubtitleClockOrigin reads the start time of the video stream the
// transcode encodes (0:v:0) from the source's headers, in milliseconds. It is
// the video stream's start, not the container's: the encoded video's first
// frame, which the segment grid counts from, carries it.
func probeSubtitleClockOrigin(ctx context.Context, opts TranscodeOpts) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, subtitleClockProbeTimeout)
	defer cancel()
	ffmpeg := opts.FFmpegPath
	if ffmpeg == "" {
		ffmpeg = ffmpegBinary()
	}
	//nolint:gosec // G204: ffprobe sits beside the configured FFmpeg; the source path is one argument, as in the transcode itself.
	out, err := exec.CommandContext(ctx, ffprobePathFromFFmpeg(ffmpeg),
		"-v", "error",
		"-analyzeduration", "3000000",
		"-probesize", "5000000",
		"-select_streams", "v:0",
		"-show_entries", "stream=start_time",
		"-of", "json",
		opts.InputPath,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %w", err)
	}
	var probe struct {
		Streams []struct {
			StartTime string `json:"start_time"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return 0, fmt.Errorf("decode ffprobe output: %w", err)
	}
	if len(probe.Streams) == 0 {
		return 0, errors.New("no video stream")
	}
	seconds, err := strconv.ParseFloat(probe.Streams[0].StartTime, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, fmt.Errorf("invalid video start time %q", probe.Streams[0].StartTime)
	}
	return int64(math.Round(seconds * 1000)), nil
}

// subtitleCueSpan is one ffmpeg generation's cue file and the segments it has
// demuxed past. A seek restart starts a new span; older spans stay readable so
// a backward seek into output an earlier generation produced keeps its cues.
type subtitleCueSpan struct {
	file  string
	start int
	// end is the last segment the generation finished, or -1 while it is the
	// live generation, whose progress is read from its own playlist.
	end int
}

// startSubtitleSpanLocked records the generation that was just spawned.
// Callers hold s.mu.
func (s *TranscodeSession) startSubtitleSpanLocked(opts TranscodeOpts) {
	if !subtitleRenditionActive(opts) || opts.subtitleCueFile == "" {
		return
	}
	s.subtitleSpans = append(s.subtitleSpans, subtitleCueSpan{file: opts.subtitleCueFile, start: opts.StartSegmentNumber, end: -1})
}

// closeSubtitleSpan freezes the live span at the last segment its process
// finished. It runs after that process has exited and before a replacement
// can write a playlist, so the playlist still belongs to the closed span.
func (s *TranscodeSession) closeSubtitleSpan() {
	head, ok := s.liveProducedHead()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.subtitleSpans {
		if s.subtitleSpans[i].end >= 0 {
			continue
		}
		if !ok || head < s.subtitleSpans[i].start {
			s.subtitleSpans = append(s.subtitleSpans[:i], s.subtitleSpans[i+1:]...)
			return
		}
		s.subtitleSpans[i].end = head
		return
	}
}

// liveProducedHead returns the last segment the current generation's own
// playlist lists. FFmpeg adds an entry only after the segment is complete, and
// a playlist an earlier generation left behind is not this process's progress.
func (s *TranscodeSession) liveProducedHead() (int, bool) {
	s.mu.Lock()
	inherited := s.inheritedManifest
	s.mu.Unlock()
	data, info, err := readManifestFile(filepath.Join(s.outputDir, "stream.m3u8"))
	if err != nil || inherited != nil && sameManifestFile(info, inherited) {
		return 0, false
	}
	head, found := -1, false
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		segNum, parseErr := ParseSegmentNumber(filepath.Base(string(bytes.TrimSpace(line))))
		if parseErr == nil && segNum > head {
			head, found = segNum, true
		}
	}
	return head, found
}

// liveSubtitleFileLocked names the live generation's cue file, or "" between
// a restart closing one span and starting the next. Callers hold s.mu.
func (s *TranscodeSession) liveSubtitleFileLocked() string {
	if n := len(s.subtitleSpans); n > 0 && s.subtitleSpans[n-1].end < 0 {
		return s.subtitleSpans[n-1].file
	}
	return ""
}

// subtitleSegmentSource finds the newest generation whose cue file (a name in
// the output directory) already holds every cue of segment segNum. imminent
// reports that a process is about to produce it: a restart is under way, or
// the live generation started at or before segNum and its output is within
// reach of it, the window in which a video request waits rather than restarts.
func (s *TranscodeSession) subtitleSegmentSource(segNum int) (file string, ready, imminent bool) {
	s.mu.Lock()
	sampledLive := s.liveSubtitleFileLocked()
	s.mu.Unlock()
	liveHead, liveOK := s.liveProducedHead()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveSubtitleFileLocked() != sampledLive {
		// A restart replaced the live generation while its playlist was
		// read: that head was the previous process's progress, not this one's.
		liveOK = false
	}
	running := s.running
	exited := !running && s.restarting == nil
	imminent = s.restarting != nil
	for i := len(s.subtitleSpans) - 1; i >= 0; i-- {
		span := s.subtitleSpans[i]
		if segNum < span.start {
			continue
		}
		if span.end < 0 && running {
			head := span.start - 1
			if liveOK {
				head = max(head, liveHead)
			}
			imminent = imminent || segNum <= head+maxSequentialMissingSegments+1
		}
		covered := span.end >= 0 && segNum <= span.end
		if span.end < 0 && liveOK {
			// A running process has demuxed past a segment once the next one
			// is complete. When it has exited, every listed segment is final.
			covered = segNum < liveHead || exited && segNum <= liveHead
		}
		if covered {
			return span.file, true, imminent
		}
	}
	return "", false, imminent
}

// SubtitleSegmentVTT returns subtitle segment segNum. A segment whose cues are
// not on disk yet is waited for, then answered as a valid empty WebVTT
// document: a missing rendition segment must never hold up player readiness
// the way a whole-file sidecar extraction did. The wait is the short one a
// video segment gets, and as long as the matching video segment's once a
// process is about to reach the segment. A stopped process still gets the
// short wait, because the player's request for the matching video segment
// restarts FFmpeg, and that restart turns this wait into the long one.
func (s *TranscodeSession) SubtitleSegmentVTT(ctx context.Context, segNum int) ([]byte, error) {
	opts := s.Opts()
	started := time.Now()
	deadline := started.Add(segmentWaitTimeout(opts.SegmentDuration))
	for {
		file, ready, imminent := s.subtitleSegmentSource(segNum)
		if imminent {
			deadline = started.Add(subtitleSegmentProductionWait)
		}
		if ready {
			document, found, err := s.cutSubtitleSegment(ctx, opts, file, segNum)
			if found || err != nil {
				return document, err
			}
		}
		if !time.Now().Before(deadline) {
			return subtitleSegmentDocument(opts, 0, segNum, nil), nil
		}
		timer := time.NewTimer(subtitleCuePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// cutSubtitleSegment cuts segment segNum from a cue file. found is false
// when the file is not on disk (yet).
func (s *TranscodeSession) cutSubtitleSegment(ctx context.Context, opts TranscodeOpts, file string, segNum int) ([]byte, bool, error) {
	data, err := s.readOutputFile(file)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read subtitle cues: %w", err)
	}
	originMS, err := s.subtitleClockOrigin(ctx)
	if err != nil {
		return nil, false, err
	}
	return subtitleSegmentDocument(opts, originMS, segNum, data), true, nil
}

// readOutputFile reads a file the session's FFmpeg wrote, confined to its
// output directory.
func (s *TranscodeSession) readOutputFile(name string) ([]byte, error) {
	root, err := os.OpenRoot(s.outputDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(name)
}

// ServeSubtitleSegment writes subtitle segment segNum.
func (s *TranscodeSession) ServeSubtitleSegment(w http.ResponseWriter, r *http.Request, segNum int) {
	body, err := s.SubtitleSegmentVTT(r.Context(), segNum)
	if err != nil && r.Context().Err() != nil {
		return
	}
	if err != nil {
		http.Error(w, "subtitle segment unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// subtitleSegmentPTSOffset90k is the shift the video segments add to the
// source clock (see HLSMPEGTSTimestampOffset90k); fMP4 keeps the source clock.
func subtitleSegmentPTSOffset90k(opts TranscodeOpts) int64 {
	if HLSOutputContainer(opts) == OutputContainerMPEGTS {
		return HLSMPEGTSTimestampOffset90k
	}
	return 0
}

// subtitleSegmentDocument cuts one segment out of a generation's cue file.
// Cues keep their absolute source times and settings; a cue that spans
// segments is repeated in each, as HLS expects, and players de-duplicate it.
// originMS is the source time of the video's first frame (see
// probeSubtitleClockOrigin): video segment N starts that far past N*d.
func subtitleSegmentDocument(opts TranscodeOpts, originMS int64, segNum int, cues []byte) []byte {
	segDur := opts.SegmentDuration
	if segDur <= 0 {
		segDur = defaultSegmentDuration
	}
	windowStart := originMS + int64(segNum)*int64(segDur)*1000
	windowEnd := windowStart + int64(segDur)*1000
	if segNum >= syntheticSegmentCount(opts.TotalDuration, segDur)-1 {
		windowEnd = math.MaxInt64
	}

	var out bytes.Buffer
	out.WriteString("WEBVTT\n")
	fmt.Fprintf(&out, "X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:%d\n", subtitleSegmentPTSOffset90k(opts)%(1<<33))

	text := strings.ReplaceAll(string(cues), "\r\n", "\n")
	if !strings.HasSuffix(text, "\n") {
		// The final cue is still being written.
		if cut := strings.LastIndex(text, "\n\n"); cut >= 0 {
			text = text[:cut]
		} else {
			text = ""
		}
	}
	for block := range strings.SplitSeq(text, "\n\n") {
		block = strings.Trim(block, "\n")
		lines := strings.Split(block, "\n")
		for i, line := range lines {
			left, right, found := strings.Cut(line, " --> ")
			if !found {
				continue
			}
			fields := strings.Fields(right)
			if len(fields) == 0 || i == len(lines)-1 {
				break
			}
			from, err := subtitleCueTimestampMS(left)
			if err != nil {
				break
			}
			to, err := subtitleCueTimestampMS(fields[0])
			if err != nil || to <= windowStart || from >= windowEnd {
				break
			}
			out.WriteByte('\n')
			out.WriteString(strings.Join(lines[i:], "\n"))
			out.WriteByte('\n')
			break
		}
	}
	return out.Bytes()
}

// subtitleCueTimestampMS parses a WebVTT [HH:]MM:SS.mmm timestamp.
func subtitleCueTimestampMS(value string) (int64, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, fmt.Errorf("invalid cue timestamp %q", value)
	}
	ms := int64(0)
	for i, part := range parts {
		if i == len(parts)-1 {
			seconds, err := strconv.ParseFloat(part, 64)
			if err != nil || seconds < 0 {
				return 0, fmt.Errorf("invalid cue timestamp %q", value)
			}
			return ms*60_000 + int64(math.Round(seconds*1000)), nil
		}
		unit, err := strconv.ParseInt(part, 10, 64)
		if err != nil || unit < 0 {
			return 0, fmt.Errorf("invalid cue timestamp %q", value)
		}
		ms = ms*60 + unit
	}
	return ms, nil
}

// ParseSubtitleSegmentNumber reports the segment number of a subtitle
// rendition segment name (sub_00012.vtt).
func ParseSubtitleSegmentNumber(name string) (int, bool) {
	name = filepath.Base(name)
	if !strings.HasPrefix(name, subtitleSegmentPrefix) || !strings.HasSuffix(name, subtitleSegmentExt) {
		return 0, false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, subtitleSegmentPrefix), subtitleSegmentExt)
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	segNum, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return segNum, true
}

// HasSubtitleRendition reports whether this session publishes a subtitle
// rendition, so master.m3u8 is a multivariant playlist.
func (s *TranscodeSession) HasSubtitleRendition() bool {
	return s.copyPlan == nil && subtitleRenditionActive(s.Opts())
}

// buildSubtitleRenditionPlaylist answers master.m3u8 for a session with a
// subtitle rendition: the multivariant playlist, or the variant or subtitle
// media playlist it points at.
func (s *TranscodeSession) buildSubtitleRenditionPlaylist(segPrefix, rawQuery string) []byte {
	rendition, query := splitSubtitleRenditionQuery(rawQuery)
	switch rendition {
	case subtitleRenditionVideo:
		return s.GenerateFullManifest(segPrefix, query)
	case subtitleRenditionSubtitles:
		return s.generateSyntheticPlaylist(segPrefix, query, subtitleSegmentPrefix, subtitleSegmentExt, false)
	default:
		return subtitleRenditionMultivariantPlaylist(s.Opts(), query)
	}
}

// subtitleRenditionMultivariantPlaylist lists the transcode as one variant
// whose SUBTITLES group holds the selected track. Both URIs are master.m3u8
// relative to itself, so the playlist works behind every relay path.
func subtitleRenditionMultivariantPlaylist(opts TranscodeOpts, query string) []byte {
	uri := func(rendition string) string {
		value := "master.m3u8?" + SubtitleRenditionQueryParam + "=" + rendition
		if query != "" {
			value += "&" + query
		}
		return value
	}
	videoKbps := opts.TargetBitrateKbps
	if videoKbps <= 0 {
		videoKbps = subtitleRenditionDefaultVideoKbps
	}
	audioKbps := opts.TargetAudioBitrateKbps
	if audioKbps <= 0 {
		audioKbps = subtitleRenditionDefaultAudioKbps
	}

	var buf bytes.Buffer
	buf.WriteString("#EXTM3U\n")
	buf.WriteString("#EXT-X-VERSION:3\n")
	buf.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	fmt.Fprintf(&buf, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=%q,NAME=%q,DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI=%q\n",
		SubtitleRenditionGroupID, SubtitleRenditionName, uri(subtitleRenditionSubtitles))
	fmt.Fprintf(&buf, "#EXT-X-STREAM-INF:BANDWIDTH=%d,SUBTITLES=%q\n", (videoKbps+audioKbps)*1000, SubtitleRenditionGroupID)
	buf.WriteString(uri(subtitleRenditionVideo) + "\n")
	return buf.Bytes()
}

// splitSubtitleRenditionQuery removes SubtitleRenditionQueryParam from a raw
// query, keeping every other parameter byte-for-byte.
func splitSubtitleRenditionQuery(rawQuery string) (rendition, rest string) {
	if rawQuery == "" {
		return "", ""
	}
	kept := make([]string, 0, strings.Count(rawQuery, "&")+1)
	for part := range strings.SplitSeq(rawQuery, "&") {
		if key, value, _ := strings.Cut(part, "="); key == SubtitleRenditionQueryParam {
			rendition = value
			continue
		}
		if part != "" {
			kept = append(kept, part)
		}
	}
	return rendition, strings.Join(kept, "&")
}
