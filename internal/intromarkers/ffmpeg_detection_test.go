package intromarkers

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestChromaprintDetectsIntroAndCreditsInGeneratedEpisodes runs real ffmpeg
// Chromaprint extraction over generated episodes that share an intro and
// credits melody and differ everywhere else, including a per-episode preview
// after the credits. It skips unless ffmpeg with the chromaprint muxer is
// available; SILO_TEST_FFMPEG names a binary other than the one on PATH.
func TestChromaprintDetectsIntroAndCreditsInGeneratedEpisodes(t *testing.T) {
	ffmpeg := os.Getenv("SILO_TEST_FFMPEG")
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Skipf("ffmpeg not found: %v", err)
	}
	ctx := context.Background()
	cfg := DefaultConfig(ffmpeg)
	extractor := NewChromaprintExtractor(cfg)
	if err := extractor.Preflight(ctx); err != nil {
		t.Skipf("ffmpeg lacks Chromaprint: %v", err)
	}

	const (
		coldOpen = 8.0
		intro    = 40.0
		credits  = 60.0
		preview  = 20.0
	)
	dir := t.TempDir()
	var candidates []Candidate
	for e, body := range []float64{352, 358, 347} {
		wav := filepath.Join(dir, fmt.Sprintf("episode-%d.wav", e+1))
		writeMelodyWAV(t, wav, []melodySection{
			{seconds: coldOpen, seed: uint64(100 + e)},
			{seconds: intro, seed: 1},
			{seconds: body, seed: uint64(200 + e)},
			{seconds: credits, seed: 2},
			{seconds: preview, seed: uint64(300 + e)},
		})
		// Library files carry lossy audio, which fingerprints less cleanly
		// than the generated PCM.
		path := filepath.Join(dir, fmt.Sprintf("episode-%d.m4a", e+1))
		if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", wav,
			"-c:a", "aac", "-b:a", "96k", path).CombinedOutput(); err != nil {
			t.Fatalf("encode %s: %v: %s", path, err, out)
		}
		candidates = append(candidates, Candidate{
			FileID:          e + 1,
			EpisodeID:       fmt.Sprintf("e%d", e+1),
			EpisodeNumber:   e + 1,
			SeasonID:        "s1",
			MediaFolderID:   1,
			FilePath:        path,
			DurationSeconds: coldOpen + intro + body + credits + preview,
		})
	}

	repo := &fakeIntroRepository{enabledLibraries: 1, eligibleCandidates: candidates}
	analyzer := &Analyzer{repo: repo, extractor: extractor, config: cfg}
	summary, err := analyzer.Run(ctx, Detection{Intros: true, Credits: true}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.FingerprintExtractionErrors != 0 {
		t.Fatalf("fingerprint extraction failed: %+v", summary)
	}

	found := map[MarkerKind]map[int]MarkerPatch{KindIntro: {}, KindCredits: {}}
	for _, patch := range repo.patches {
		found[patch.Kind][patch.FileID] = patch
		t.Logf("file %d %s %.2f-%.2f confidence %.2f %s", patch.FileID, patch.Kind, patch.Start, patch.End, patch.Confidence, patch.Algorithm)
	}
	const tolerance = 2.5
	for _, candidate := range candidates {
		introPatch, ok := found[KindIntro][candidate.FileID]
		if !ok {
			t.Fatalf("file %d: no intro detected", candidate.FileID)
		}
		if math.Abs(introPatch.Start-coldOpen) > tolerance || math.Abs(introPatch.End-(coldOpen+intro)) > tolerance {
			t.Errorf("file %d intro = %.2f-%.2f, want about %.0f-%.0f",
				candidate.FileID, introPatch.Start, introPatch.End, coldOpen, coldOpen+intro)
		}
		creditsPatch, ok := found[KindCredits][candidate.FileID]
		if !ok {
			t.Fatalf("file %d: no credits detected", candidate.FileID)
		}
		wantStart := candidate.DurationSeconds - preview - credits
		wantEnd := candidate.DurationSeconds - preview
		if math.Abs(creditsPatch.Start-wantStart) > tolerance || math.Abs(creditsPatch.End-wantEnd) > tolerance {
			t.Errorf("file %d credits = %.2f-%.2f, want about %.0f-%.0f",
				candidate.FileID, creditsPatch.Start, creditsPatch.End, wantStart, wantEnd)
		}
		if creditsPatch.Algorithm != CreditsChromaprintAlgorithm {
			t.Errorf("file %d credits algorithm = %q", candidate.FileID, creditsPatch.Algorithm)
		}
	}
}

type melodySection struct {
	seconds float64
	seed    uint64
}

// writeMelodyWAV writes mono 16-bit PCM of random note sequences, one per
// section. Sections with the same seed are identical audio; Chromaprint
// fingerprints pitch content, so different seeds do not match.
func writeMelodyWAV(t *testing.T, path string, sections []melodySection) {
	t.Helper()
	const (
		sampleRate    = 11025
		noteSamples   = sampleRate / 4
		baseFrequency = 220.0
	)
	var samples []int16
	for _, section := range sections {
		rng := rand.New(rand.NewPCG(section.seed, 42))
		total := int(section.seconds * sampleRate)
		var frequency float64
		for i := range total {
			if i%noteSamples == 0 {
				frequency = baseFrequency * math.Pow(2, float64(rng.IntN(36))/12)
			}
			phase := float64(i%noteSamples) / float64(noteSamples)
			envelope := math.Min(1, math.Min(phase*20, (1-phase)*20))
			x := 2 * math.Pi * frequency * float64(i) / sampleRate
			value := envelope * (0.6*math.Sin(x) + 0.25*math.Sin(2*x) + 0.1*math.Sin(3*x))
			samples = append(samples, int16(value*12000))
		}
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	dataSize := uint32(len(samples) * 2)
	header := []any{
		[4]byte{'R', 'I', 'F', 'F'}, 36 + dataSize, [4]byte{'W', 'A', 'V', 'E'},
		[4]byte{'f', 'm', 't', ' '}, uint32(16), uint16(1), uint16(1),
		uint32(sampleRate), uint32(sampleRate * 2), uint16(2), uint16(16),
		[4]byte{'d', 'a', 't', 'a'}, dataSize,
	}
	for _, field := range header {
		if err := binary.Write(f, binary.LittleEndian, field); err != nil {
			t.Fatal(err)
		}
	}
	if err := binary.Write(f, binary.LittleEndian, samples); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
