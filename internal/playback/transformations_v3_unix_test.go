//go:build unix

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
