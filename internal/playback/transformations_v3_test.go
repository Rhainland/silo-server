package playback

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestH264EncoderAvailabilityMatchesSelectedExecutor(t *testing.T) {
	cases := []struct {
		name    string
		listing string
		hwAccel string
		want    bool
	}{
		{"software", " V....D libx264 H.264", "none", true},
		{"qsv", " V..... h264_qsv H.264\n V....D libx264 H.264", "qsv", true},
		{"vaapi", " V..... h264_vaapi H.264\n V....D libx264 H.264", "vaapi", true},
		{"nvenc", " V....D h264_nvenc H.264\n V....D libx264 H.264", "nvenc", true},
		{"hardware_without_software_fallback", " V..... h264_qsv H.264", "qsv", false},
		{"wrong_encoder", " V....D libx264 H.264", "nvenc", false},
		{"videotoolbox_not_selected", " V..... h264_videotoolbox H.264", "none", false},
		{"substring_is_not_an_encoder", " V....D libx264_fake H.264", "none", false},
	}
	for _, value := range cases {
		t.Run(value.name, func(t *testing.T) {
			if got := h264EncoderAvailableForExecutorV3([]byte(value.listing), value.hwAccel); got != value.want {
				t.Fatalf("h264EncoderAvailableForExecutorV3 = %v, want %v", got, value.want)
			}
		})
	}
}

func TestH264FiltersAvailabilityMatchesSelectedExecutor(t *testing.T) {
	cases := []struct {
		name, listing, hwAccel string
		want                   bool
	}{
		{"software", "", "none", true},
		{"qsv", "hwupload scale_vaapi hwmap", "qsv", true},
		{"qsv_missing_map", "hwupload scale_vaapi", "qsv", false},
		{"vaapi", "hwupload scale_vaapi", "vaapi", true},
		{"nvenc", "hwupload_cuda scale_cuda", "nvenc", true},
		{"nvenc_missing_upload", "scale_cuda", "nvenc", false},
		{"substring_is_not_a_filter", "hwupload_cuda_fake scale_cuda", "nvenc", false},
	}
	for _, value := range cases {
		t.Run(value.name, func(t *testing.T) {
			if got := h264FiltersAvailableForExecutorV3([]byte(value.listing), value.hwAccel); got != value.want {
				t.Fatalf("h264FiltersAvailableForExecutorV3 = %v, want %v", got, value.want)
			}
		})
	}
}

func TestProbeTransformationRegistryV3AdvertisesVideoToH264RecipeVersion3(t *testing.T) {
	ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\ncase \"$2\" in\n-bsfs) echo dovi_rpu ;;\n-encoders) echo ' V....D libx264 H.264'; echo ' A....D aac AAC' ;;\nesac\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	registry := ProbeTransformationRegistryV3(context.Background(), ffmpeg)
	for _, transformation := range registry.Advertised() {
		if transformation.Name == TransformationVideoToH264V3 {
			if transformation.RecipeVersion != "3" {
				t.Fatalf("video_to_h264 recipe version = %q, want 3", transformation.RecipeVersion)
			}
			return
		}
	}
	t.Fatal("video_to_h264 was not advertised")
}

func TestProbeTransformationRegistryCachesAnUnobservedProbeBrieflyThenRetries(t *testing.T) {
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	marker := filepath.Join(dir, "fail-encoders")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  case \"$arg\" in\n" +
		"    -encoders) if [ -e " + marker + " ]; then exit 1; fi; echo ' V....D libx264 H.264'; echo ' A....D aac AAC'; exit 0 ;;\n" +
		"    -filters) echo 'hwupload hwmap scale_vaapi hwupload_cuda scale_cuda'; exit 0 ;;\n" +
		"    -bsfs) echo 'dovi_rpu'; exit 0 ;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(ffmpegPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub ffmpeg: %v", err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	failed := ProbeTransformationRegistryForExecutorV3(context.Background(), ffmpegPath, "none")
	if failed.ProbeObserved() {
		t.Fatal("failed probe unexpectedly marked observed")
	}
	if failed.Available(TransformationVideoToH264V3) {
		t.Fatal("a failed encoder probe must not advertise the H.264 transform")
	}

	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	brieflyCached := ProbeTransformationRegistryForExecutorV3(context.Background(), ffmpegPath, "none")
	if brieflyCached.ProbeObserved() || brieflyCached.Available(TransformationVideoToH264V3) {
		t.Fatal("incomplete observation was not reused during its negative TTL")
	}
	transformationRegistryCacheV3.Lock()
	for key, entry := range transformationRegistryCacheV3.values {
		if !entry.registry.ProbeObserved() {
			entry.expiresAt = time.Now().Add(-time.Second)
			transformationRegistryCacheV3.values[key] = entry
		}
	}
	transformationRegistryCacheV3.Unlock()
	recovered := ProbeTransformationRegistryForExecutorV3(context.Background(), ffmpegPath, "none")
	if !recovered.Available(TransformationVideoToH264V3) || !recovered.Available(TransformationAudioToAACV3) {
		t.Fatalf("transient probe failure was cached: %#v", recovered.Advertised())
	}

	// A fully-observed answer is still memoized, so the hot paths that probe
	// per request do not fork ffmpeg three times each.
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("rewrite marker: %v", err)
	}
	cached := ProbeTransformationRegistryForExecutorV3(context.Background(), ffmpegPath, "none")
	if !cached.Available(TransformationVideoToH264V3) {
		t.Fatal("a successful probe was not memoized")
	}
}

func TestProbeTransformationRegistryCachesCompleteNegativeObservation(t *testing.T) {
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	encodersPath := filepath.Join(dir, "encoders")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do case \"$arg\" in " +
		"-encoders) cat " + encodersPath + " ;; " +
		"-filters) echo scale ;; " +
		"-bsfs) echo dovi_rpu ;; esac; done\n"
	if err := os.WriteFile(ffmpegPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(encodersPath, []byte(" A....D aac AAC\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	negative := ProbeTransformationRegistryForExecutorV3(context.Background(), ffmpegPath, "none")
	if !negative.ProbeObserved() || negative.Available(TransformationVideoToH264V3) {
		t.Fatalf("initial complete-negative probe = %#v", negative.Advertised())
	}
	if err := os.WriteFile(encodersPath, []byte(" V....D libx264 H.264\n A....D aac AAC\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cached := ProbeTransformationRegistryForExecutorV3(context.Background(), ffmpegPath, "none")
	if cached.Available(TransformationVideoToH264V3) {
		t.Fatal("complete negative observation was unexpectedly re-probed")
	}
}

func TestProbeTransformationRegistrySingleFlightsConcurrentCallers(t *testing.T) {
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	callsPath := filepath.Join(dir, "calls")
	startedPath := filepath.Join(dir, "started")
	releasePath := filepath.Join(dir, "release")
	if err := syscall.Mkfifo(startedPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(releasePath, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do case \"$arg\" in -bsfs|-encoders|-filters) " +
		"echo \"$arg\" >> " + callsPath + "; " +
		"if [ \"$arg\" = -bsfs ]; then echo started > " + startedPath + "; read ignored < " + releasePath + "; fi; " +
		"case \"$arg\" in -bsfs) echo dovi_rpu ;; -encoders) echo ' V....D libx264 H.264'; echo ' A....D aac AAC' ;; -filters) echo scale ;; esac; exit 0 ;; esac; done\n"
	if err := os.WriteFile(ffmpegPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	const callers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			_ = ProbeTransformationRegistryForExecutorV3(context.Background(), ffmpegPath, "none")
		}()
	}
	probeStarted := make(chan error, 1)
	go func() {
		_, err := os.ReadFile(startedPath)
		probeStarted <- err
	}()
	close(start)
	select {
	case err := <-probeStarted:
		if err != nil {
			t.Fatalf("wait for probe leader: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("probe leader did not start")
	}
	if err := os.WriteFile(releasePath, []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	contents, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(string(contents))); got != 3 {
		t.Fatalf("ffmpeg probe command count = %d, want 3", got)
	}
}

func TestExecutionValidationToleratesOnlyIncompleteProbe(t *testing.T) {
	required := []TransformationV3{{Name: TransformationVideoToH264V3, Executor: ExecutorServerV3, RecipeVersion: TransformationVideoToH264RecipeVersionV3}}
	if err := ValidateRequiredTransformationsForExecutionV3(required, newTransformationRegistryV3(nil, false)); err != nil {
		t.Fatalf("incomplete probe rejected execution: %v", err)
	}
	if err := ValidateRequiredTransformationsForExecutionV3(required, NewTransformationRegistryV3(nil)); err == nil {
		t.Fatal("complete negative probe did not reject execution")
	}
}
