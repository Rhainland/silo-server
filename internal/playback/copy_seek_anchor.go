package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

type copySeekProbePacket struct {
	PTSSeconds string `json:"pts_time"`
	DTSSeconds string `json:"dts_time"`
	Flags      string `json:"flags"`
}

type copySeekProbeOutput struct {
	Packets []copySeekProbePacket `json:"packets"`
}

// ResolveCopySeekAnchor returns the keyframe timestamp FFmpeg's input seek will
// actually use for a copy-video restart. FFmpeg cannot discard the pre-roll
// between that keyframe and requestedSeekSeconds while preserving -c:v copy,
// so callers need both timestamps: requestedSeekSeconds remains the -ss input,
// while the returned anchor defines the stream's real timeline origin.
//
// The tiny read interval is intentional. ffprobe asks the demuxer to seek to
// requestedSeekSeconds, which lands on the same preceding key packet FFmpeg
// uses, then emits only the packets at that seek point instead of scanning the
// media from the beginning.
func ResolveCopySeekAnchor(
	ctx context.Context,
	ffmpegPath string,
	inputPath string,
	requestedSeekSeconds float64,
	segmentDuration int,
) (float64, int, error) {
	if requestedSeekSeconds <= 0 {
		return 0, 0, nil
	}
	if strings.TrimSpace(inputPath) == "" {
		return 0, 0, fmt.Errorf("resolve copy seek anchor: empty input path")
	}
	if segmentDuration <= 0 {
		segmentDuration = DefaultSegmentDuration
	}

	interval := strconv.FormatFloat(requestedSeekSeconds, 'f', 6, 64) + "%+0.001"
	cmd := exec.CommandContext(ctx, ffprobePathFromFFmpeg(ffmpegPath),
		"-v", "error",
		"-select_streams", "v:0",
		"-read_intervals", interval,
		"-show_entries", "packet=pts_time,dts_time,flags",
		"-of", "json",
		inputPath,
	)
	var stdout bytes.Buffer
	stderr := newBoundedTailBuffer(stderrTailMaxBytes)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if tail := truncateStderr(stderr.String()); tail != "" {
			return 0, 0, fmt.Errorf("resolve copy seek anchor: ffprobe failed: %w (stderr: %s)", err, tail)
		}
		return 0, 0, fmt.Errorf("resolve copy seek anchor: ffprobe failed: %w", err)
	}

	var output copySeekProbeOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return 0, 0, fmt.Errorf("resolve copy seek anchor: decode ffprobe output: %w", err)
	}
	for _, packet := range output.Packets {
		if !strings.Contains(packet.Flags, "K") {
			continue
		}
		timestamp := packet.PTSSeconds
		if timestamp == "" || strings.EqualFold(timestamp, "N/A") {
			timestamp = packet.DTSSeconds
		}
		anchor, err := strconv.ParseFloat(timestamp, 64)
		if err != nil || math.IsNaN(anchor) || math.IsInf(anchor, 0) {
			continue
		}
		anchor = math.Max(0, anchor)
		return anchor, int(anchor / float64(segmentDuration)), nil
	}

	return 0, 0, fmt.Errorf("resolve copy seek anchor: ffprobe returned no key packet")
}
