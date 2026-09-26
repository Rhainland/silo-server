package intromarkers

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestDetectChapterCreditsEndsAtFollowingChapter(t *testing.T) {
	segment, ok := DetectChapterCredits([]models.MediaChapter{
		{Index: 0, Title: "Opening", StartSeconds: 0, EndSeconds: 90},
		{Index: 1, Title: "Part A", StartSeconds: 90, EndSeconds: 1300},
		{Index: 2, Title: "Ending", StartSeconds: 1300, EndSeconds: 1390},
		{Index: 3, Title: "Preview", StartSeconds: 1390, EndSeconds: 1420},
	}, 1420)
	if !ok {
		t.Fatal("expected credits chapter")
	}
	if segment.Start != 1300 || segment.End != 1390 {
		t.Fatalf("credits = %.0f-%.0f, want 1300-1390 so the preview is not skipped", segment.Start, segment.End)
	}
	if segment.Algorithm != CreditsChapterAlgorithm || segment.Confidence != 0.95 {
		t.Fatalf("unexpected metadata %+v", segment)
	}
}

func TestDetectChapterCreditsLastChapterRunsToEndOfFile(t *testing.T) {
	segment, ok := DetectChapterCredits([]models.MediaChapter{
		{Index: 0, Title: "Part 1", StartSeconds: 0, EndSeconds: 2500},
		{Index: 1, Title: "End Credits", StartSeconds: 2500, EndSeconds: 0},
	}, 2580)
	if !ok || segment.Start != 2500 || segment.End != 2580 {
		t.Fatalf("credits = %+v ok=%t, want 2500-2580", segment, ok)
	}
}

func TestDetectChapterCreditsMergesAdjacentCreditsChapters(t *testing.T) {
	segment, ok := DetectChapterCredits([]models.MediaChapter{
		{Index: 0, Title: "Episode", StartSeconds: 0, EndSeconds: 1200},
		{Index: 1, Title: "ED", StartSeconds: 1200, EndSeconds: 1290},
		{Index: 2, Title: "Credits", StartSeconds: 1290, EndSeconds: 1320},
	}, 1320)
	if !ok || segment.Start != 1200 || segment.End != 1320 {
		t.Fatalf("credits = %+v ok=%t, want the merged 1200-1320", segment, ok)
	}
}

// Scenes around the credits stay playable: a post-credits scene after the
// credits is neither merged into them nor taken as the credits.
func TestDetectChapterCreditsKeepsCreditsScenes(t *testing.T) {
	for _, scene := range []string{"Post Credits Scene", "Post-Credits Scene", "After Credits", "Mid-credits Stinger", "Credits Scene", "Credits Tag"} {
		t.Run(scene, func(t *testing.T) {
			segment, ok := DetectChapterCredits([]models.MediaChapter{
				{Title: "Episode", StartSeconds: 0, EndSeconds: 1300},
				{Title: "End Credits", StartSeconds: 1300, EndSeconds: 1390},
				{Title: scene, StartSeconds: 1390, EndSeconds: 1420},
			}, 1420)
			if !ok || segment.Start != 1300 || segment.End != 1390 {
				t.Fatalf("credits = %+v ok=%t, want 1300-1390 with %q left playable", segment, ok, scene)
			}
		})
	}
}

func TestDetectChapterCreditsAcceptsCreditsTitles(t *testing.T) {
	for _, title := range []string{"Credits", "End Credits", "Closing Credits", "Ending Credits", "Ending", "Ending Theme", "ED", "ED2", "Outro", "End Titles", "Ending: Song Name"} {
		t.Run(title, func(t *testing.T) {
			if _, ok := DetectChapterCredits([]models.MediaChapter{
				{Title: "Episode", StartSeconds: 0, EndSeconds: 1300},
				{Title: title, StartSeconds: 1300, EndSeconds: 1390},
			}, 1390); !ok {
				t.Fatalf("title %q should be credits", title)
			}
		})
	}
}

func TestDetectChapterCreditsRejectsNonCredits(t *testing.T) {
	tests := map[string][]models.MediaChapter{
		"opening credits are the intro": {
			{Title: "Opening Credits", StartSeconds: 0, EndSeconds: 60},
			{Title: "Episode", StartSeconds: 60, EndSeconds: 1400},
		},
		"credits chapter in the first half": {
			{Title: "Credits", StartSeconds: 100, EndSeconds: 160},
			{Title: "Episode", StartSeconds: 160, EndSeconds: 1400},
		},
		"generated and lookalike titles": {
			{Title: "Chapter 1", StartSeconds: 0, EndSeconds: 1300},
			{Title: "Edited Scene", StartSeconds: 1300, EndSeconds: 1400},
		},
		"story chapters named after an ending": {
			{Title: "Episode", StartSeconds: 0, EndSeconds: 1300},
			{Title: "The Ending", StartSeconds: 1300, EndSeconds: 1340},
			{Title: "Alternate Ending", StartSeconds: 1340, EndSeconds: 1400},
		},
		"too short": {
			{Title: "Episode", StartSeconds: 0, EndSeconds: 1395},
			{Title: "Credits", StartSeconds: 1395, EndSeconds: 1400},
		},
	}
	for name, chapters := range tests {
		t.Run(name, func(t *testing.T) {
			if segment, ok := DetectChapterCredits(chapters, 1400); ok {
				t.Fatalf("unexpected credits %+v", segment)
			}
		})
	}
}

// creditsSeasonInputs builds ending-window fingerprints for a season whose
// episodes share creditsPoints of credits audio starting creditsFromEnd seconds
// before the end of each file. Durations differ so each file's window starts
// somewhere else.
func creditsSeasonInputs(cfg Config, durations []float64, creditsFromEnd float64, creditsPoints int) []fingerprintInput {
	shared := make([]uint32, creditsPoints)
	sharedRNG := rand.New(rand.NewPCG(0, 3))
	for i := range shared {
		shared[i] = sharedRNG.Uint32()
	}
	inputs := make([]fingerprintInput, 0, len(durations))
	for e, duration := range durations {
		windowStart, windowEnd := fingerprintWindow(KindCredits, duration, cfg)
		points := make([]uint32, int((windowEnd-windowStart)/DefaultPointHopSeconds))
		rng := rand.New(rand.NewPCG(uint64(e+1), 11))
		for i := range points {
			points[i] = rng.Uint32()
		}
		at := int((duration - creditsFromEnd - windowStart) / DefaultPointHopSeconds)
		copy(points[at:], shared)
		inputs = append(inputs, fingerprintInput{
			Candidate: Candidate{FileID: e + 1, EpisodeID: fmt.Sprintf("e%d", e+1), EpisodeNumber: e + 1, DurationSeconds: duration},
			Points:    points,
		})
	}
	return inputs
}

func TestCompareCreditsFingerprintsMapsEndingWindowToFileTime(t *testing.T) {
	cfg := DefaultConfig("ffmpeg")
	// Each ending window starts at a different offset from the credits, and
	// most offsets fall between the eight-point steps a sparse shift search
	// samples.
	durations := []float64{1800, 1812, 1795, 1806}
	// 600 points is about 74 seconds of credits, followed by a preview of
	// about 26 seconds that differs per episode.
	segments := compareCreditsFingerprints(creditsSeasonInputs(cfg, durations, 100, 600), cfg)
	if len(segments) != len(durations) {
		t.Fatalf("matched %d files, want %d", len(segments), len(durations))
	}
	for e, duration := range durations {
		segment := segments[e+1]
		wantStart := duration - 100
		wantEnd := wantStart + 600*DefaultPointHopSeconds
		if math.Abs(segment.Start-wantStart) > 2 || math.Abs(segment.End-wantEnd) > 2 {
			t.Fatalf("file %d credits = %.2f-%.2f, want about %.2f-%.2f", e+1, segment.Start, segment.End, wantStart, wantEnd)
		}
		if segment.Algorithm != CreditsChromaprintAlgorithm {
			t.Fatalf("file %d algorithm = %q, want %q", e+1, segment.Algorithm, CreditsChromaprintAlgorithm)
		}
		if segment.Confidence != chromaprintConsistentConfidence {
			t.Fatalf("file %d confidence = %.2f, want season-consistent %.2f", e+1, segment.Confidence, chromaprintConsistentConfidence)
		}
	}
}

func TestCompareCreditsFingerprintsRunToEndOfFile(t *testing.T) {
	cfg := DefaultConfig("ffmpeg")
	durations := []float64{1800, 1800, 1800}
	// The shared credits fill the last 60 seconds of every file.
	segments := compareCreditsFingerprints(creditsSeasonInputs(cfg, durations, 60, 487), cfg)
	for e, duration := range durations {
		segment, ok := segments[e+1]
		if !ok {
			t.Fatalf("file %d has no credits", e+1)
		}
		if segment.End != duration {
			t.Fatalf("file %d credits end %.2f, want the end of the file %.0f", e+1, segment.End, duration)
		}
	}
}

// Credits and intros keep separate fingerprint caches and season states, so
// enabling one never discards or reuses the other's work.
func TestCreditsKeysDifferFromIntroKeys(t *testing.T) {
	cfg := DefaultConfig("ffmpeg")
	if cfg.fingerprintKey(KindCredits) == cfg.fingerprintKey(KindIntro) {
		t.Fatal("credits and intro fingerprints share a cache key")
	}
	if cfg.analysisKey(KindCredits) == cfg.analysisKey(KindIntro) {
		t.Fatal("credits and intro season analysis share a state key")
	}
	if cfg.fingerprintKey(KindIntro) != cfg.ConfigHash() {
		t.Fatal("the intro fingerprint key must stay ConfigHash so stored fingerprints remain valid")
	}
	start, end := fingerprintWindow(KindCredits, 1800, cfg)
	if start != 1350 || end != 1800 {
		t.Fatalf("credits window = %.0f-%.0f, want the last quarter 1350-1800", start, end)
	}
}

func TestLoadDetectionDefaultsToBothKinds(t *testing.T) {
	tests := []struct {
		values map[string]string
		want   Detection
	}{
		{values: map[string]string{}, want: Detection{Intros: true, Credits: true}},
		{values: map[string]string{"markers.detect_intros": "false"}, want: Detection{Credits: true}},
		{values: map[string]string{"markers.detect_credits": "false"}, want: Detection{Intros: true}},
		{values: map[string]string{"markers.detect_intros": "false", "markers.detect_credits": "false"}, want: Detection{}},
	}
	for _, tt := range tests {
		got, err := LoadDetection(context.Background(), mapSettings(tt.values))
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Fatalf("LoadDetection(%v) = %+v, want %+v", tt.values, got, tt.want)
		}
	}
}

type mapSettings map[string]string

func (m mapSettings) Get(_ context.Context, key string) (string, error) { return m[key], nil }

// kindExtractor serves fixed fingerprints per file and records which windows
// were requested.
type kindExtractor struct {
	mu        sync.Mutex
	points    map[MarkerKind]map[int][]uint32
	kinds     []MarkerKind
	preflight int
}

func (e *kindExtractor) Preflight(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.preflight++
	return nil
}

func (e *kindExtractor) Extract(_ context.Context, candidate Candidate, kind MarkerKind) (Fingerprint, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.kinds = append(e.kinds, kind)
	points := e.points[kind][candidate.FileID]
	if len(points) == 0 {
		return Fingerprint{}, false, nil
	}
	return Fingerprint{MediaFileID: candidate.FileID, Points: points}, true, nil
}

// creditsRunFixture is a three-episode season: the first episode has an
// authored credits chapter, and all three share credits audio at the end.
func creditsRunFixture(t *testing.T) (*fakeIntroRepository, *kindExtractor) {
	t.Helper()
	cfg := DefaultConfig("ffmpeg")
	inputs := creditsSeasonInputs(cfg, []float64{1800, 1800, 1800}, 100, 600)
	extractor := &kindExtractor{points: map[MarkerKind]map[int][]uint32{KindCredits: {}}}
	var candidates []Candidate
	for _, input := range inputs {
		candidate := input.Candidate
		candidate.SeasonID = "s1"
		candidate.MediaFolderID = 1
		candidates = append(candidates, candidate)
		extractor.points[KindCredits][candidate.FileID] = input.Points
	}
	candidates[0].Chapters = []models.MediaChapter{
		{Title: "Episode", StartSeconds: 0, EndSeconds: 1700},
		{Title: "End Credits", StartSeconds: 1700, EndSeconds: 1774},
		{Title: "Preview", StartSeconds: 1774, EndSeconds: 1800},
	}
	return &fakeIntroRepository{enabledLibraries: 1, eligibleCandidates: candidates}, extractor
}

func TestRunDetectsCreditsFromChaptersAndChromaprint(t *testing.T) {
	repo, extractor := creditsRunFixture(t)
	analyzer := &Analyzer{repo: repo, extractor: extractor, config: DefaultConfig("ffmpeg")}

	summary, err := analyzer.Run(context.Background(), Detection{Credits: true}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.CreditsChapterMarkersWritten != 1 || summary.CreditsChromaprintWritten != 3 {
		t.Fatalf("summary = %+v, want one chapter and three Chromaprint credits", summary)
	}
	for _, kind := range extractor.kinds {
		if kind != KindCredits {
			t.Fatalf("extracted a %s window with only credits detection selected", kind)
		}
	}
	byAlgorithm := map[string]int{}
	for _, patch := range repo.patches {
		if patch.Kind != KindCredits {
			t.Fatalf("patch %+v is not a credits patch", patch)
		}
		if patch.Source != models.MarkerSourceScanner {
			t.Fatalf("patch source = %q, want scanner", patch.Source)
		}
		byAlgorithm[patch.Algorithm]++
		if patch.Algorithm == CreditsChapterAlgorithm && (patch.Start != 1700 || patch.End != 1774) {
			t.Fatalf("chapter credits = %.0f-%.0f, want 1700-1774", patch.Start, patch.End)
		}
	}
	if byAlgorithm[CreditsChapterAlgorithm] != 1 || byAlgorithm[CreditsChromaprintAlgorithm] != 3 {
		t.Fatalf("patches by algorithm = %v", byAlgorithm)
	}
	if len(repo.upsertedStates) != 1 || repo.upsertedStates[0].Kind != KindCredits {
		t.Fatalf("season states = %+v, want one credits state", repo.upsertedStates)
	}
}

func TestRunIntrosOnlyNeverTouchesCredits(t *testing.T) {
	repo, extractor := creditsRunFixture(t)
	analyzer := &Analyzer{repo: repo, extractor: extractor, config: DefaultConfig("ffmpeg")}

	summary, err := analyzer.Run(context.Background(), Detection{Intros: true}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, kind := range extractor.kinds {
		if kind == KindCredits {
			t.Fatal("extracted an ending window with credits detection off")
		}
	}
	for _, patch := range repo.patches {
		if patch.Kind == KindCredits {
			t.Fatalf("wrote credits %+v with credits detection off", patch)
		}
	}
	if summary.CreditsChapterMarkersWritten != 0 || summary.CreditsChromaprintWritten != 0 {
		t.Fatalf("summary = %+v, want no credits", summary)
	}
}

func TestRunWithNothingSelectedDoesNothing(t *testing.T) {
	repo, extractor := creditsRunFixture(t)
	analyzer := &Analyzer{repo: repo, extractor: extractor, config: DefaultConfig("ffmpeg")}

	if _, err := analyzer.Run(context.Background(), Detection{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(repo.patches) != 0 || len(extractor.kinds) != 0 || extractor.preflight != 0 {
		t.Fatalf("patches=%d extractions=%d preflights=%d, want none", len(repo.patches), len(extractor.kinds), extractor.preflight)
	}
}

func TestRunBothKindsChecksChromaprintOnce(t *testing.T) {
	repo, extractor := creditsRunFixture(t)
	analyzer := &Analyzer{repo: repo, extractor: extractor, config: DefaultConfig("ffmpeg")}

	if _, err := analyzer.Run(context.Background(), Detection{Intros: true, Credits: true}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if extractor.preflight != 1 {
		t.Fatalf("preflight calls = %d, want 1", extractor.preflight)
	}
}

func TestRunLeavesOnlineCreditsAlone(t *testing.T) {
	repo, extractor := creditsRunFixture(t)
	online := models.MarkerSourceOnline
	start, end := 1690.0, 1770.0
	repo.eligibleCandidates[1].CreditsStart = &start
	repo.eligibleCandidates[1].CreditsEnd = &end
	repo.eligibleCandidates[1].CreditsMarkersSource = &online
	analyzer := &Analyzer{repo: repo, extractor: extractor, config: DefaultConfig("ffmpeg")}

	if _, err := analyzer.Run(context.Background(), Detection{Credits: true}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, patch := range repo.patches {
		if patch.FileID == 2 {
			t.Fatalf("patched online credits of file 2: %+v", patch)
		}
	}
	if len(repo.patches) == 0 {
		t.Fatal("the other files must still get credits")
	}
}

func TestAnalyzeEpisodeCreditsPatchesOnlyThatEpisode(t *testing.T) {
	repo, extractor := creditsRunFixture(t)
	repo.episodeCandidates = map[string][]Candidate{"e2": {repo.eligibleCandidates[1]}}
	repo.groupCandidates = map[string][]Candidate{
		groupKey(1, "s1", repo.eligibleCandidates[1].AnalysisGroupKey()): repo.eligibleCandidates,
	}
	analyzer := &Analyzer{repo: repo, extractor: extractor, config: DefaultConfig("ffmpeg")}

	summary, err := analyzer.AnalyzeEpisode(context.Background(), "e2", Detection{Credits: true})
	if err != nil {
		t.Fatalf("AnalyzeEpisode: %v", err)
	}
	if summary.CreditsChromaprintWritten != 1 {
		t.Fatalf("summary = %+v, want one credits marker", summary)
	}
	if len(repo.patches) != 1 || repo.patches[0].FileID != 2 || repo.patches[0].Kind != KindCredits {
		t.Fatalf("patches = %+v, want credits for file 2 only", repo.patches)
	}
}

func TestMarkerUpdateForCreditsPatchLeavesIntroUntouched(t *testing.T) {
	update, err := markerUpdateForPatch(MarkerPatch{
		Kind: KindCredits, FileID: 1, Start: 1700, End: 1774,
		Source: models.MarkerSourceScanner, Confidence: 0.9, Algorithm: CreditsChromaprintAlgorithm,
	})
	if err != nil {
		t.Fatal(err)
	}
	if update.IntroStart != nil || update.IntroEnd != nil {
		t.Fatalf("credits patch wrote an intro: %+v", update)
	}
	if update.CreditsStart == nil || *update.CreditsStart != 1700 || update.CreditsEnd == nil || *update.CreditsEnd != 1774 {
		t.Fatalf("credits patch wrote %+v", update)
	}
	if update.MarkersAlgorithm != CreditsChromaprintAlgorithm {
		t.Fatalf("algorithm = %q", update.MarkersAlgorithm)
	}
}

// compareCreditsFingerprints compares ending-window fingerprints with the
// credits rules.
func compareCreditsFingerprints(inputs []fingerprintInput, cfg Config) map[int]Segment {
	return compareFingerprints(inputs, cfg, compareSpecFor(KindCredits, cfg))
}

func TestDetectionKindsForFile(t *testing.T) {
	scanner, online := models.MarkerSourceScanner, models.MarkerSourceOnline
	start, end := 10.0, 50.0
	both := Detection{Intros: true, Credits: true}
	file := &models.MediaFile{
		IntroStart: &start, IntroEnd: &end, IntroMarkersSource: &online,
		CreditsStart: &start, CreditsEnd: &end, CreditsMarkersSource: &scanner,
	}
	if got := both.Missing(file); got != (Detection{}) {
		t.Fatalf("Missing = %+v, want nothing: both kinds are present", got)
	}
	if got := both.Redetectable(file); got != (Detection{Credits: true}) {
		t.Fatalf("Redetectable = %+v, want only the detector's own credits", got)
	}
	legacy := &models.MediaFile{IntroStart: &start, IntroEnd: &end, MarkersSource: &online}
	if got := both.Redetectable(legacy); got != (Detection{Credits: true}) {
		t.Fatalf("Redetectable(legacy online intro) = %+v, want credits only", got)
	}
	if got := (Detection{Intros: true}).Union(Detection{Credits: true}); got != both {
		t.Fatalf("Union = %+v", got)
	}
}

// Snapping to chapters and to the end of the file may lengthen credits past
// the match limit; the lengthened result must still count.
func TestCreditsSpecAcceptsSnappedMaximumLengthCredits(t *testing.T) {
	cfg := DefaultConfig("ffmpeg")
	spec := compareSpecFor(KindCredits, cfg)
	// The start snaps 4.5 seconds earlier to the chapter, and the end, about
	// five seconds short of the end of the file, snaps to it.
	candidate := Candidate{DurationSeconds: 3499, Chapters: []models.MediaChapter{{Title: "Credits", StartSeconds: 3040, EndSeconds: 3499}}}
	windowStart, _ := fingerprintWindow(KindCredits, candidate.DurationSeconds, cfg)
	matchStart := 3044.5 - windowStart - chromaprintStartLeadSeconds
	match := Segment{Start: matchStart, End: matchStart + spec.maxMatch}
	adjusted := adjustCreditsSegment(match, candidate, windowStart)
	if adjusted.End-adjusted.Start <= spec.maxMatch+creditsEndSnapSeconds+chromaprintEndLeadSeconds {
		t.Fatalf("fixture did not lengthen the match: %+v", adjusted)
	}
	if !spec.valid(adjusted) {
		t.Fatalf("adjusted credits %.2f-%.2f (%.2fs) rejected", adjusted.Start, adjusted.End, adjusted.End-adjusted.Start)
	}
}

// "Ending" and "Outro" also name story chapters. A long one is the story's
// final scene, not credits, and stays out of the credits run.
func TestDetectChapterCreditsLimitsAmbiguousTitles(t *testing.T) {
	segment, ok := DetectChapterCredits([]models.MediaChapter{
		{Title: "Part 3", StartSeconds: 1200, EndSeconds: 2280},
		{Title: "Ending", StartSeconds: 2280, EndSeconds: 2520},
		{Title: "Credits", StartSeconds: 2520, EndSeconds: 2640},
	}, 2640)
	if !ok || segment.Start != 2520 || segment.End != 2640 {
		t.Fatalf("credits = %+v ok=%t, want only the 2520-2640 credits chapter", segment, ok)
	}
	if _, ok := DetectChapterCredits([]models.MediaChapter{
		{Title: "Part 3", StartSeconds: 1200, EndSeconds: 2280},
		{Title: "Outro", StartSeconds: 2280, EndSeconds: 2640},
	}, 2640); ok {
		t.Fatal("a six-minute chapter titled Outro was taken as credits")
	}
	if _, ok := DetectChapterCredits([]models.MediaChapter{
		{Title: "Part B", StartSeconds: 700, EndSeconds: 1330},
		{Title: "Ending", StartSeconds: 1330, EndSeconds: 1420},
	}, 1420); !ok {
		t.Fatal("a 90-second anime Ending chapter was not taken as credits")
	}
}

// A marker with no recorded source is left alone by refresh, as the analyzer
// leaves it alone.
func TestRedetectableLeavesMarkersWithoutSource(t *testing.T) {
	start, end := 10.0, 50.0
	file := &models.MediaFile{IntroStart: &start, IntroEnd: &end}
	if got := (Detection{Intros: true, Credits: true}).Redetectable(file); got != (Detection{Credits: true}) {
		t.Fatalf("Redetectable = %+v, want the missing credits only", got)
	}
	if !(Detection{Intros: true, Credits: true}).Covers(Detection{Credits: true}) || (Detection{Intros: true}).Covers(Detection{Credits: true}) {
		t.Fatal("Covers is wrong")
	}
}
