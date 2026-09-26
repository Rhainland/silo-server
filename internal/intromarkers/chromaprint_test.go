package intromarkers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeRawPointsKeepsBinaryWhitespaceBytes(t *testing.T) {
	output := []byte{0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0a}

	points := decodeRawPoints(output)

	want := []uint32{0x20, 0x0a000000}
	if !reflect.DeepEqual(points, want) {
		t.Fatalf("decodeRawPoints() = %#v, want %#v", points, want)
	}
}

func TestChromaprintExtractorUsesSingleFFmpegThreadPerProcess(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "ffmpeg-args.txt")
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
printf '\001\000\000\000'
`, argsPath)
	if err := os.WriteFile(ffmpegPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}

	cfg := DefaultConfig(ffmpegPath)
	cfg.MaxParallelFFmpeg = 4
	extractor := NewChromaprintExtractor(cfg)

	_, ok, err := extractor.Extract(context.Background(), Candidate{
		FileID:          42,
		FilePath:        "/tmp/episode.mkv",
		DurationSeconds: 1200,
	}, KindIntro)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if !ok {
		t.Fatal("Extract returned no fingerprint")
	}

	argsBytes, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake ffmpeg args: %v", err)
	}
	args := strings.Fields(string(argsBytes))
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-threads" {
			if args[i+1] != "1" {
				t.Fatalf("expected ffmpeg -threads 1, got %q", args[i+1])
			}
			return
		}
	}

	t.Fatalf("expected ffmpeg args to contain -threads, got %q", string(argsBytes))
}
