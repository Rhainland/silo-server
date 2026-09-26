package handlers

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Silo-Server/silo-server/internal/intromarkers"
	"github.com/Silo-Server/silo-server/internal/markers"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

type fakePlaybackMarkerProvider struct{}

func (fakePlaybackMarkerProvider) ID() string { return "fake-online" }

func (fakePlaybackMarkerProvider) FetchMarkers(context.Context, markers.Request) (markers.Result, error) {
	return markers.Result{}, nil
}

type fakePlaybackIntroAnalyzer struct {
	mu      sync.Mutex
	calls   int
	detects []intromarkers.Detection
	started chan struct{}
	release chan struct{}
	onCall  func()
	summary intromarkers.RunSummary
	err     error
}

func (a *fakePlaybackIntroAnalyzer) AnalyzeEpisode(_ context.Context, _ string, detect intromarkers.Detection) (intromarkers.RunSummary, error) {
	a.mu.Lock()
	a.calls++
	a.detects = append(a.detects, detect)
	a.mu.Unlock()
	if a.onCall != nil {
		a.onCall()
	}
	if a.started != nil {
		select {
		case a.started <- struct{}{}:
		default:
		}
	}
	if a.release != nil {
		<-a.release
	}
	summary := a.summary
	if summary.FilesConsidered == 0 {
		summary = intromarkers.RunSummary{FilesConsidered: 1, ChapterMarkersWritten: 1}
	}
	return summary, a.err
}

func (a *fakePlaybackIntroAnalyzer) detections() []intromarkers.Detection {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]intromarkers.Detection(nil), a.detects...)
}

func (a *fakePlaybackIntroAnalyzer) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type fakePlaybackIntroEligibility struct {
	eligible bool
	err      error
}

func (e fakePlaybackIntroEligibility) IntroDetectionEligibleForPlayback(context.Context, int) (bool, error) {
	return e.eligible, e.err
}

func (e fakePlaybackIntroEligibility) IsFileInEnabledLibrary(context.Context, int) (bool, error) {
	return e.eligible, e.err
}

type fakePlaybackMarkerFileResolver struct {
	mu   sync.Mutex
	file *models.MediaFile
}

func (r *fakePlaybackMarkerFileResolver) GetByID(context.Context, int) (*models.MediaFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil, nil
	}
	cp := *r.file
	return &cp, nil
}

func (r *fakePlaybackMarkerFileResolver) setFile(file *models.MediaFile) {
	r.mu.Lock()
	r.file = file
	r.mu.Unlock()
}

type fakePlaybackMarkerNotifier struct {
	ch chan *models.MediaFile
}

func (n fakePlaybackMarkerNotifier) MarkersUpdated(_ context.Context, file *models.MediaFile) {
	if n.ch == nil {
		return
	}
	n.ch <- file
}

func TestMaybeQueueLazyPlaybackMarkersGates(t *testing.T) {
	tests := []struct {
		name     string
		lazy     string
		mode     string
		file     *models.MediaFile
		settings map[string]string
		eligible bool
	}{
		{
			name:     "lazy disabled",
			lazy:     "false",
			mode:     "local",
			file:     lazyMarkerTestFile(),
			eligible: true,
		},
		{
			name:     "mode off",
			lazy:     "true",
			mode:     "off",
			file:     lazyMarkerTestFile(),
			eligible: true,
		},
		{
			name:     "online mode without providers",
			lazy:     "true",
			mode:     "online",
			file:     lazyMarkerTestFile(),
			eligible: true,
		},
		{
			name: "intro and credits already present",
			lazy: "true",
			mode: "local",
			file: func() *models.MediaFile {
				file := lazyMarkerTestFile()
				start, end := 10.0, 60.0
				creditsStart, creditsEnd := 1700.0, 1800.0
				file.IntroStart, file.IntroEnd = &start, &end
				file.CreditsStart, file.CreditsEnd = &creditsStart, &creditsEnd
				return file
			}(),
			eligible: true,
		},
		{
			name: "intro present and credits detection off",
			lazy: "true",
			mode: "local",
			file: func() *models.MediaFile {
				file := lazyMarkerTestFile()
				start, end := 10.0, 60.0
				file.IntroStart, file.IntroEnd = &start, &end
				return file
			}(),
			settings: map[string]string{markers.SettingDetectCredits: "false"},
			eligible: true,
		},
		{
			name:     "intro and credits detection off",
			lazy:     "true",
			mode:     "local",
			file:     lazyMarkerTestFile(),
			settings: map[string]string{markers.SettingDetectIntros: "false", markers.SettingDetectCredits: "false"},
			eligible: true,
		},
		{
			name: "missing episode id",
			lazy: "true",
			mode: "local",
			file: func() *models.MediaFile {
				file := lazyMarkerTestFile()
				file.EpisodeID = ""
				return file
			}(),
			eligible: true,
		},
		{
			name:     "library ineligible",
			lazy:     "true",
			mode:     "local",
			file:     lazyMarkerTestFile(),
			eligible: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analyzer := &fakePlaybackIntroAnalyzer{started: make(chan struct{}, 1)}
			handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), &fakePlaybackMarkerFileResolver{file: tt.file})
			values := map[string]string{
				markers.SettingLazyPlayback: tt.lazy,
				markers.SettingMode:         tt.mode,
			}
			for key, value := range tt.settings {
				values[key] = value
			}
			handler.SettingsRepo = testPlaybackSettingsRepo{values: values}
			handler.IntroRepository = fakePlaybackIntroEligibility{eligible: tt.eligible}
			handler.IntroAnalyzer = analyzer
			handler.MarkerLazyContext = context.Background()

			handler.maybeQueueLazyPlaybackMarkers(context.Background(), &playback.Session{ID: "session-1"}, tt.file)

			select {
			case <-analyzer.started:
				t.Fatal("AnalyzeEpisode started, want gated")
			case <-time.After(25 * time.Millisecond):
			}
			if got := analyzer.callCount(); got != 0 {
				t.Fatalf("AnalyzeEpisode calls = %d, want 0", got)
			}
		})
	}
}

func TestMaybeQueueLazyPlaybackMarkersLocalModeRunsAnalyzerAndEmitsUpdate(t *testing.T) {
	file := lazyMarkerTestFile()
	resolver := &fakePlaybackMarkerFileResolver{file: file}
	start := 12.0
	end := 75.0
	analyzer := &fakePlaybackIntroAnalyzer{
		started: make(chan struct{}, 1),
		onCall: func() {
			updated := *file
			updated.IntroStart = &start
			updated.IntroEnd = &end
			resolver.setFile(&updated)
		},
	}
	notifier := fakePlaybackMarkerNotifier{ch: make(chan *models.MediaFile, 1)}
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), resolver)
	handler.SettingsRepo = testPlaybackSettingsRepo{values: map[string]string{
		markers.SettingLazyPlayback: "true",
		markers.SettingMode:         "local",
	}}
	handler.IntroRepository = fakePlaybackIntroEligibility{eligible: true}
	handler.IntroAnalyzer = analyzer
	handler.MarkerUpdateNotifier = notifier
	handler.MarkerLazyContext = context.Background()

	handler.maybeQueueLazyPlaybackMarkers(context.Background(), &playback.Session{ID: "session-1"}, file)

	select {
	case <-analyzer.started:
	case <-time.After(time.Second):
		t.Fatal("AnalyzeEpisode did not start")
	}

	select {
	case notified := <-notifier.ch:
		if notified.ID != file.ID {
			t.Fatalf("notified file ID = %d, want %d", notified.ID, file.ID)
		}
		if notified.IntroStart == nil || notified.IntroEnd == nil {
			t.Fatalf("notified file missing intro marker: %#v", notified)
		}
	case <-time.After(time.Second):
		t.Fatal("marker update was not emitted")
	}
	if got := analyzer.callCount(); got != 1 {
		t.Fatalf("AnalyzeEpisode calls = %d, want 1", got)
	}
}

func TestMaybeQueueLazyPlaybackMarkersBothModeFallsBackToLocalWithoutProviders(t *testing.T) {
	analyzer := &fakePlaybackIntroAnalyzer{started: make(chan struct{}, 1)}
	file := lazyMarkerTestFile()
	handler := newLazyMarkerTestHandler(file, analyzer, nil)
	handler.SettingsRepo = testPlaybackSettingsRepo{values: map[string]string{
		markers.SettingLazyPlayback: "true",
		markers.SettingMode:         "both",
	}}

	handler.maybeQueueLazyPlaybackMarkers(context.Background(), &playback.Session{ID: "session-1"}, file)

	select {
	case <-analyzer.started:
	case <-time.After(time.Second):
		t.Fatal("AnalyzeEpisode did not start")
	}
	if got := analyzer.callCount(); got != 1 {
		t.Fatalf("AnalyzeEpisode calls = %d, want 1", got)
	}
}

type playbackMarkerPopulationFunc func(context.Context, *models.MediaFile) (*models.MediaFile, bool, error)

func (f playbackMarkerPopulationFunc) Populate(ctx context.Context, file *models.MediaFile) (*models.MediaFile, bool, error) {
	return f(ctx, file)
}

func TestOnDemandPlaybackMarkersHonorsLocalAnalysisSetting(t *testing.T) {
	for _, lazy := range []string{"false", "true"} {
		t.Run(lazy, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				file := lazyMarkerTestFile()
				analyzer := &fakePlaybackIntroAnalyzer{}
				handler := newLazyMarkerTestHandler(file, analyzer, nil)
				handler.MarkerLazyContext = t.Context()
				handler.SettingsRepo = testPlaybackSettingsRepo{values: map[string]string{
					markers.SettingLazyPlayback:  lazy,
					markers.SettingMode:          "both",
					markers.SettingOnlineStorage: "on_demand",
				}}
				handler.MarkerRegistry = markers.NewRegistry(slog.Default())
				if err := handler.MarkerRegistry.Register(fakePlaybackMarkerProvider{}); err != nil {
					t.Fatal(err)
				}
				lookups := 0
				handler.MarkerPopulation = playbackMarkerPopulationFunc(func(_ context.Context, file *models.MediaFile) (*models.MediaFile, bool, error) {
					lookups++
					return file, false, nil
				})

				handler.maybeQueueLazyPlaybackMarkers(t.Context(), &playback.Session{ID: "session-1"}, file)
				synctest.Wait()

				if lookups != 1 {
					t.Fatalf("online lookups = %d, want 1", lookups)
				}
				wantLocal := 0
				if lazy == "true" {
					wantLocal = 1
				}
				if got := analyzer.callCount(); got != wantLocal {
					t.Fatalf("local analysis calls = %d, want %d", got, wantLocal)
				}
			})
		})
	}
}

func TestMaybeQueueLazyPlaybackMarkersDedupesInFlightFile(t *testing.T) {
	analyzer := &fakePlaybackIntroAnalyzer{started: make(chan struct{}, 1), release: make(chan struct{})}
	file := lazyMarkerTestFile()
	handler := newLazyMarkerTestHandler(file, analyzer, nil)

	session := &playback.Session{ID: "session-1"}
	handler.maybeQueueLazyPlaybackMarkers(context.Background(), session, file)
	handler.maybeQueueLazyPlaybackMarkers(context.Background(), session, file)
	time.Sleep(25 * time.Millisecond)

	if got := analyzer.callCount(); got != 1 {
		t.Fatalf("AnalyzeEpisode calls = %d, want 1", got)
	}
	close(analyzer.release)
}

func TestMaybeQueueLazyPlaybackMarkersOnlineModeWithProviderDoesNotRunLocalAnalyzer(t *testing.T) {
	analyzer := &fakePlaybackIntroAnalyzer{started: make(chan struct{}, 1)}
	file := lazyMarkerTestFile()
	handler := newLazyMarkerTestHandler(file, analyzer, nil)
	registry := markers.NewRegistry(slog.Default())
	if err := registry.Register(fakePlaybackMarkerProvider{}); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	handler.MarkerRegistry = registry
	handler.SettingsRepo = testPlaybackSettingsRepo{values: map[string]string{
		markers.SettingLazyPlayback: "true",
		markers.SettingMode:         "online",
	}}

	handler.maybeQueueLazyPlaybackMarkers(context.Background(), &playback.Session{ID: "session-1"}, file)

	select {
	case <-analyzer.started:
		t.Fatal("AnalyzeEpisode started, want online-only provider mode to skip local analyzer")
	case <-time.After(25 * time.Millisecond):
	}
	if got := analyzer.callCount(); got != 0 {
		t.Fatalf("AnalyzeEpisode calls = %d, want 0", got)
	}
}

func TestMaybeQueueLazyPlaybackMarkersAnalyzerSuccessWithoutMarkerDoesNotEmitUpdate(t *testing.T) {
	analyzer := &fakePlaybackIntroAnalyzer{started: make(chan struct{}, 1)}
	file := lazyMarkerTestFile()
	notifier := fakePlaybackMarkerNotifier{ch: make(chan *models.MediaFile, 1)}
	handler := newLazyMarkerTestHandler(file, analyzer, notifier)

	handler.maybeQueueLazyPlaybackMarkers(context.Background(), &playback.Session{ID: "session-1"}, file)

	select {
	case <-analyzer.started:
	case <-time.After(time.Second):
		t.Fatal("AnalyzeEpisode did not start")
	}
	select {
	case notified := <-notifier.ch:
		t.Fatalf("unexpected marker update: %#v", notified)
	case <-time.After(25 * time.Millisecond):
	}
}

func newLazyMarkerTestHandler(
	file *models.MediaFile,
	analyzer *fakePlaybackIntroAnalyzer,
	notifier PlaybackMarkerUpdateNotifier,
) *PlaybackHandler {
	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), &fakePlaybackMarkerFileResolver{file: file})
	handler.SettingsRepo = testPlaybackSettingsRepo{values: map[string]string{
		markers.SettingLazyPlayback: "true",
		markers.SettingMode:         "local",
	}}
	handler.IntroRepository = fakePlaybackIntroEligibility{eligible: true}
	handler.IntroAnalyzer = analyzer
	handler.MarkerUpdateNotifier = notifier
	handler.MarkerLazyContext = context.Background()
	return handler
}

func lazyMarkerTestFile() *models.MediaFile {
	return &models.MediaFile{
		ID:            42,
		EpisodeID:     "episode-1",
		MediaFolderID: 7,
		Duration:      1800,
	}
}

// An intro already saved, for example from an online provider, must not stop
// local credits detection: only the missing kind is detected.
func TestMaybeQueueLazyPlaybackMarkersDetectsOnlyMissingKinds(t *testing.T) {
	tests := []struct {
		name     string
		intro    bool
		credits  bool
		settings map[string]string
		want     intromarkers.Detection
	}{
		{name: "nothing saved", want: intromarkers.Detection{Intros: true, Credits: true}},
		{name: "intro saved", intro: true, want: intromarkers.Detection{Credits: true}},
		{name: "credits saved", credits: true, want: intromarkers.Detection{Intros: true}},
		{
			name:     "intro detection off",
			settings: map[string]string{markers.SettingDetectIntros: "false"},
			want:     intromarkers.Detection{Credits: true},
		},
		{
			name:     "credits detection off",
			settings: map[string]string{markers.SettingDetectCredits: "false"},
			want:     intromarkers.Detection{Intros: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := lazyMarkerTestFile()
			if tt.intro {
				start, end := 10.0, 60.0
				file.IntroStart, file.IntroEnd = &start, &end
			}
			if tt.credits {
				start, end := 1700.0, 1800.0
				file.CreditsStart, file.CreditsEnd = &start, &end
			}
			analyzer := &fakePlaybackIntroAnalyzer{started: make(chan struct{}, 1)}
			handler := newLazyMarkerTestHandler(file, analyzer, nil)
			values := map[string]string{
				markers.SettingLazyPlayback: "true",
				markers.SettingMode:         "local",
			}
			for key, value := range tt.settings {
				values[key] = value
			}
			handler.SettingsRepo = testPlaybackSettingsRepo{values: values}

			handler.maybeQueueLazyPlaybackMarkers(context.Background(), &playback.Session{ID: "session-1"}, file)

			select {
			case <-analyzer.started:
			case <-time.After(time.Second):
				t.Fatal("AnalyzeEpisode did not start")
			}
			if got := analyzer.detections(); len(got) != 1 || got[0] != tt.want {
				t.Fatalf("AnalyzeEpisode calls = %+v, want [%+v]", got, tt.want)
			}
		})
	}
}

// An on-demand online intro is never saved. Detecting the missing credits must
// not replace it with a local intro, and the update sent after detection must
// still carry it.
func TestOnDemandPlaybackMarkersKeepOnlineIntro(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		file := lazyMarkerTestFile()
		resolver := &fakePlaybackMarkerFileResolver{file: file}
		creditsStart, creditsEnd := 1700.0, 1780.0
		analyzer := &fakePlaybackIntroAnalyzer{onCall: func() {
			stored := *file
			stored.CreditsStart, stored.CreditsEnd = &creditsStart, &creditsEnd
			stored.MarkerSegments = []models.MarkerSegment{{Kind: "credits", StartSeconds: creditsStart, EndSeconds: creditsEnd}}
			resolver.setFile(&stored)
		}}
		notifier := fakePlaybackMarkerNotifier{ch: make(chan *models.MediaFile, 4)}
		handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), resolver)
		handler.SettingsRepo = testPlaybackSettingsRepo{values: map[string]string{
			markers.SettingLazyPlayback:  "true",
			markers.SettingMode:          "both",
			markers.SettingOnlineStorage: "on_demand",
		}}
		handler.IntroRepository = fakePlaybackIntroEligibility{eligible: true}
		handler.IntroAnalyzer = analyzer
		handler.MarkerUpdateNotifier = notifier
		handler.MarkerLazyContext = t.Context()
		handler.MarkerRegistry = markers.NewRegistry(slog.Default())
		if err := handler.MarkerRegistry.Register(fakePlaybackMarkerProvider{}); err != nil {
			t.Fatal(err)
		}
		online := models.MarkerSourceOnline
		introStart, introEnd := 20.0, 80.0
		handler.MarkerPopulation = playbackMarkerPopulationFunc(func(_ context.Context, file *models.MediaFile) (*models.MediaFile, bool, error) {
			overlay := *file
			overlay.IntroStart, overlay.IntroEnd, overlay.IntroMarkersSource = &introStart, &introEnd, &online
			overlay.MarkerSegments = []models.MarkerSegment{{Kind: "intro", StartSeconds: introStart, EndSeconds: introEnd}}
			return &overlay, true, nil
		})

		handler.maybeQueueLazyPlaybackMarkers(t.Context(), &playback.Session{ID: "session-1"}, file)
		synctest.Wait()

		got := analyzer.detections()
		if len(got) != 1 || got[0] != (intromarkers.Detection{Credits: true}) {
			t.Fatalf("AnalyzeEpisode calls = %+v, want credits only", got)
		}
		var last *models.MediaFile
		for len(notifier.ch) > 0 {
			last = <-notifier.ch
		}
		if last == nil {
			t.Fatal("no marker update was sent")
		}
		if last.IntroStart == nil || *last.IntroStart != introStart || last.IntroMarkersSource == nil || *last.IntroMarkersSource != online {
			t.Fatalf("final update lost the online intro: %+v", last)
		}
		if last.CreditsStart == nil || *last.CreditsStart != creditsStart {
			t.Fatalf("final update lacks the detected credits: %+v", last)
		}
		kinds := map[string]bool{}
		for _, segment := range last.MarkerSegments {
			kinds[segment.Kind] = true
		}
		if !kinds["intro"] || !kinds["credits"] {
			t.Fatalf("final update segments = %+v, want intro and credits", last.MarkerSegments)
		}
	})
}

// Playback does not re-run detection of a kind it tried for the file within
// the retry interval: a season comparison that found no credits finds none on
// the next start either.
func TestLazyDetectionLogThrottlesEachKind(t *testing.T) {
	var log lazyDetectionLog
	both := intromarkers.Detection{Intros: true, Credits: true}
	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if got := log.take(1, intromarkers.Detection{Credits: true}, start); got != (intromarkers.Detection{Credits: true}) {
		t.Fatalf("first credits attempt = %+v", got)
	}
	if got := log.take(1, both, start.Add(time.Hour)); got != (intromarkers.Detection{Intros: true}) {
		t.Fatalf("within the interval = %+v, want only the untried intro", got)
	}
	if got := log.take(1, both, start.Add(2*time.Hour)); got.Any() {
		t.Fatalf("both tried recently = %+v, want nothing", got)
	}
	if got := log.take(2, both, start.Add(2*time.Hour)); got != both {
		t.Fatalf("another file = %+v, want both", got)
	}
	if got := log.take(1, both, start.Add(lazyDetectionRetryInterval+2*time.Hour)); got != both {
		t.Fatalf("after the interval = %+v, want both again", got)
	}
}

func TestMaybeQueueLazyPlaybackMarkersDoesNotRepeatWithinRetryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		file := lazyMarkerTestFile()
		analyzer := &fakePlaybackIntroAnalyzer{}
		handler := newLazyMarkerTestHandler(file, analyzer, nil)
		handler.MarkerLazyContext = t.Context()
		for range 3 {
			handler.maybeQueueLazyPlaybackMarkers(t.Context(), &playback.Session{ID: "session-1"}, file)
			synctest.Wait()
		}
		if got := analyzer.callCount(); got != 1 {
			t.Fatalf("local analysis ran %d times across three starts, want once", got)
		}
	})
}
