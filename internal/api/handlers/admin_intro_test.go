package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Silo-Server/silo-server/internal/intromarkers"
	"github.com/Silo-Server/silo-server/internal/markers"
	"github.com/Silo-Server/silo-server/internal/models"
)

type fakeIntroAnalyzer struct {
	started  chan string
	detected chan intromarkers.Detection
	release  chan struct{}
	summary  intromarkers.RunSummary
	err      error
}

func (f *fakeIntroAnalyzer) AnalyzeEpisode(ctx context.Context, episodeID string, detect intromarkers.Detection) (intromarkers.RunSummary, error) {
	if f.detected != nil {
		f.detected <- detect
	}
	if f.started != nil {
		f.started <- episodeID
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return intromarkers.RunSummary{}, ctx.Err()
		}
	}
	if f.err != nil {
		return intromarkers.RunSummary{}, f.err
	}
	if f.summary.FilesConsidered != 0 {
		return f.summary, nil
	}
	return intromarkers.RunSummary{FilesConsidered: 1}, nil
}

type fakeIntroEligibility struct {
	result *intromarkers.EpisodeIntroEligibility
	err    error
}

func (f fakeIntroEligibility) EpisodeIntroEligibility(context.Context, string) (*intromarkers.EpisodeIntroEligibility, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type fakeMarkerSettings struct {
	values map[string]string
	err    error
}

func (f fakeMarkerSettings) Get(_ context.Context, key string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.values[key], nil
}

type fakeAdminIntroFileResolver struct {
	files  []*models.MediaFile
	err    error
	called chan string
}

func (f fakeAdminIntroFileResolver) GetByEpisodeID(_ context.Context, episodeID string) ([]*models.MediaFile, error) {
	if f.called != nil {
		f.called <- episodeID
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.files, nil
}

type fakeAdminIntroMarkerNotifier struct {
	ch chan *models.MediaFile
}

type markerRefreshFunc func(context.Context, *models.MediaFile) (*models.MediaFile, bool, error)

func (f markerRefreshFunc) Refresh(ctx context.Context, file *models.MediaFile) (*models.MediaFile, bool, error) {
	return f(ctx, file)
}

func TestAdminMarkerRefreshOnlineDoesNotRequireLocalDetection(t *testing.T) {
	started := make(chan int, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := NewAdminIntroHandler(nil, nil, ctx, nil)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeOnline)}}
	handler.FileResolver = fakeAdminIntroFileResolver{files: []*models.MediaFile{{ID: 42, EpisodeID: "ep1"}}}
	handler.OnlineMarkers = markerRefreshFunc(func(ctx context.Context, file *models.MediaFile) (*models.MediaFile, bool, error) {
		started <- file.ID
		<-ctx.Done()
		return file, false, ctx.Err()
	})
	status, err := handler.RefreshEpisodeMarkers(t.Context(), "ep1", "refresh-v2")
	if err != nil || status != "queued" {
		t.Fatalf("refresh: status=%q err=%v", status, err)
	}
	select {
	case id := <-started:
		if id != 42 {
			t.Fatalf("refreshed file %d, want 42", id)
		}
	case <-time.After(time.Second):
		t.Fatal("online refresh did not start")
	}
	status, err = handler.RefreshEpisodeMarkers(t.Context(), "ep1", "refresh-v2")
	if err != nil || status != "already_running" {
		t.Fatalf("duplicate refresh: status=%q err=%v", status, err)
	}
}

func (n fakeAdminIntroMarkerNotifier) MarkersUpdated(_ context.Context, file *models.MediaFile) {
	if n.ch == nil {
		return
	}
	n.ch <- file
}

func TestAdminIntroRedetectQueuesAndDedupsInFlightEpisode(t *testing.T) {
	analyzer := &fakeIntroAnalyzer{
		started: make(chan string, 1),
		release: make(chan struct{}),
	}
	handler := NewAdminIntroHandler(
		analyzer,
		fakeIntroEligibility{result: &intromarkers.EpisodeIntroEligibility{
			EpisodeID:             "ep1",
			HasMediaFiles:         true,
			IntroDetectionEnabled: true,
		}},
		context.Background(),
		nil,
	)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeLocal)}}
	router := chi.NewRouter()
	router.Post("/admin/items/{id}/redetect-intro", handler.HandleRedetectEpisodeIntro)

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/admin/items/ep1/redetect-intro", nil))
	if first.Code != http.StatusAccepted {
		t.Fatalf("expected first request 202, got %d: %s", first.Code, first.Body.String())
	}
	if status := decodeRedetectStatus(t, first); status != "queued" {
		t.Fatalf("expected queued, got %q", status)
	}

	select {
	case id := <-analyzer.started:
		if id != "ep1" {
			t.Fatalf("expected analyzer to start ep1, got %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("analyzer did not start")
	}

	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/admin/items/ep1/redetect-intro", nil))
	if second.Code != http.StatusAccepted {
		t.Fatalf("expected second request 202, got %d: %s", second.Code, second.Body.String())
	}
	if status := decodeRedetectStatus(t, second); status != "already_running" {
		t.Fatalf("expected already_running, got %q", status)
	}

	close(analyzer.release)
}

func TestAdminIntroRedetectRejectsModesWithoutLocalAnalysis(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode string
	}{
		{name: "off", mode: string(markers.ModeOff)},
		{name: "online", mode: string(markers.ModeOnline)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			analyzer := &fakeIntroAnalyzer{started: make(chan string, 1), release: make(chan struct{})}
			handler := NewAdminIntroHandler(
				analyzer,
				fakeIntroEligibility{result: &intromarkers.EpisodeIntroEligibility{
					EpisodeID:             "ep1",
					HasMediaFiles:         true,
					IntroDetectionEnabled: true,
				}},
				context.Background(),
				nil,
			)
			handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: tt.mode}}
			router := chi.NewRouter()
			router.Post("/admin/items/{id}/redetect-intro", handler.HandleRedetectEpisodeIntro)

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/items/ep1/redetect-intro", nil))
			if rec.Code != http.StatusConflict {
				t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
			}

			select {
			case id := <-analyzer.started:
				t.Fatalf("expected analyzer not to start, got %q", id)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func waitForDetection(t *testing.T, analyzer *fakeIntroAnalyzer) intromarkers.Detection {
	t.Helper()
	select {
	case got := <-analyzer.detected:
		return got
	case <-time.After(time.Second):
		t.Fatal("analyzer did not start")
		return intromarkers.Detection{}
	}
}

func eligibleEpisode() fakeIntroEligibility {
	return fakeIntroEligibility{result: &intromarkers.EpisodeIntroEligibility{
		EpisodeID: "ep1", HasMediaFiles: true, IntroDetectionEnabled: true,
	}}
}

// The frozen v1 routes predate credits detection: they keep detecting intros
// only, and the detection settings do not reject them.
func TestAdminIntroRedetectKeepsDetectingIntrosOnly(t *testing.T) {
	analyzer := &fakeIntroAnalyzer{detected: make(chan intromarkers.Detection, 1)}
	handler := NewAdminIntroHandler(analyzer, eligibleEpisode(), context.Background(), nil)
	handler.Settings = fakeMarkerSettings{values: map[string]string{
		markers.SettingMode:          string(markers.ModeLocal),
		markers.SettingDetectIntros:  "false",
		markers.SettingDetectCredits: "false",
	}}
	router := chi.NewRouter()
	router.Post("/admin/items/{id}/redetect-intro", handler.HandleRedetectEpisodeIntro)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/items/ep1/redetect-intro", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if got, want := waitForDetection(t, analyzer), (intromarkers.Detection{Intros: true}); got != want {
		t.Fatalf("detection = %+v, want %+v", got, want)
	}
}

// The v2 re-detect operation behind the web "Re-detect Markers" action detects
// what the detection settings select.
func TestAdminMarkerRedetectV2FollowsDetectionSettings(t *testing.T) {
	for _, tt := range []struct {
		name            string
		intros, credits string
		want            intromarkers.Detection
	}{
		{name: "intros only", intros: "true", credits: "false", want: intromarkers.Detection{Intros: true}},
		{name: "credits only", intros: "false", credits: "true", want: intromarkers.Detection{Credits: true}},
		{name: "both", intros: "true", credits: "true", want: intromarkers.Detection{Intros: true, Credits: true}},
		{name: "unset means both", want: intromarkers.Detection{Intros: true, Credits: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			analyzer := &fakeIntroAnalyzer{detected: make(chan intromarkers.Detection, 1)}
			handler := NewAdminIntroHandler(analyzer, eligibleEpisode(), context.Background(), nil)
			values := map[string]string{markers.SettingMode: string(markers.ModeBoth)}
			if tt.intros != "" {
				values[markers.SettingDetectIntros] = tt.intros
			}
			if tt.credits != "" {
				values[markers.SettingDetectCredits] = tt.credits
			}
			handler.Settings = fakeMarkerSettings{values: values}
			if status, err := handler.RefreshEpisodeMarkers(context.Background(), "ep1", "redetect-v2"); err != nil || status != "queued" {
				t.Fatalf("status=%q err=%v", status, err)
			}
			if got := waitForDetection(t, analyzer); got != tt.want {
				t.Fatalf("detection = %+v, want %+v", got, tt.want)
			}
		})
	}

	off := &fakeIntroAnalyzer{detected: make(chan intromarkers.Detection, 1)}
	handler := NewAdminIntroHandler(off, eligibleEpisode(), context.Background(), nil)
	handler.Settings = fakeMarkerSettings{values: map[string]string{
		markers.SettingMode:          string(markers.ModeBoth),
		markers.SettingDetectIntros:  "false",
		markers.SettingDetectCredits: "false",
	}}
	_, err := handler.RefreshEpisodeMarkers(context.Background(), "ep1", "redetect-v2")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Fatalf("both kinds off: err = %v, want 409", err)
	}
}

func TestAdminMarkerRefreshV2LocalFollowsDetectionSettings(t *testing.T) {
	refresh := func(values map[string]string, analyzer *fakeIntroAnalyzer) (string, error) {
		handler := NewAdminIntroHandler(analyzer, eligibleEpisode(), context.Background(), nil)
		values[markers.SettingMode] = string(markers.ModeLocal)
		handler.Settings = fakeMarkerSettings{values: values}
		handler.FileResolver = fakeAdminIntroFileResolver{files: []*models.MediaFile{{ID: 42, EpisodeID: "ep1"}}}
		return handler.RefreshEpisodeMarkers(context.Background(), "ep1", "refresh-v2")
	}

	off := &fakeIntroAnalyzer{detected: make(chan intromarkers.Detection, 1)}
	_, err := refresh(map[string]string{markers.SettingDetectIntros: "false", markers.SettingDetectCredits: "false"}, off)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Fatalf("both kinds off: err = %v, want 409", err)
	}

	creditsOnly := &fakeIntroAnalyzer{detected: make(chan intromarkers.Detection, 1)}
	if status, err := refresh(map[string]string{markers.SettingDetectIntros: "false"}, creditsOnly); err != nil || status != "queued" {
		t.Fatalf("credits only: status=%q err=%v", status, err)
	}
	if got, want := waitForDetection(t, creditsOnly), (intromarkers.Detection{Credits: true}); got != want {
		t.Fatalf("detection = %+v, want %+v", got, want)
	}
}

// In both mode an explicit refresh re-runs markers this server detected, and
// leaves the kinds an online provider supplied alone.
func TestAdminMarkerRefreshV2BothRedetectsOwnMarkers(t *testing.T) {
	scanner, online := models.MarkerSourceScanner, models.MarkerSourceOnline
	introStart, introEnd := 10.0, 50.0
	creditsStart, creditsEnd := 1700.0, 1760.0
	analyzer := &fakeIntroAnalyzer{detected: make(chan intromarkers.Detection, 1)}
	handler := NewAdminIntroHandler(analyzer, eligibleEpisode(), context.Background(), nil)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeBoth)}}
	handler.FileResolver = fakeAdminIntroFileResolver{files: []*models.MediaFile{{ID: 42, EpisodeID: "ep1"}}}
	handler.OnlineMarkers = markerRefreshFunc(func(_ context.Context, file *models.MediaFile) (*models.MediaFile, bool, error) {
		refreshed := *file
		refreshed.IntroStart, refreshed.IntroEnd, refreshed.IntroMarkersSource = &introStart, &introEnd, &scanner
		refreshed.CreditsStart, refreshed.CreditsEnd, refreshed.CreditsMarkersSource = &creditsStart, &creditsEnd, &online
		return &refreshed, false, nil
	})
	if status, err := handler.RefreshEpisodeMarkers(context.Background(), "ep1", "refresh-v2"); err != nil || status != "queued" {
		t.Fatalf("refresh: status=%q err=%v", status, err)
	}
	if got, want := waitForDetection(t, analyzer), (intromarkers.Detection{Intros: true}); got != want {
		t.Fatalf("detection = %+v, want the detector's own intro only %+v", got, want)
	}
}

func TestAdminIntroRedetectRejectsNonEpisode(t *testing.T) {
	handler := NewAdminIntroHandler(
		&fakeIntroAnalyzer{started: make(chan string, 1), release: make(chan struct{})},
		fakeIntroEligibility{err: intromarkers.ErrEpisodeNotFound},
		context.Background(),
		nil,
	)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeLocal)}}
	router := chi.NewRouter()
	router.Post("/admin/items/{id}/redetect-intro", handler.HandleRedetectEpisodeIntro)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/items/movie1/redetect-intro", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminIntroRedetectRejectsIntroDisabledLibrary(t *testing.T) {
	handler := NewAdminIntroHandler(
		&fakeIntroAnalyzer{started: make(chan string, 1), release: make(chan struct{})},
		fakeIntroEligibility{result: &intromarkers.EpisodeIntroEligibility{
			EpisodeID:             "ep1",
			HasMediaFiles:         true,
			IntroDetectionEnabled: false,
		}},
		context.Background(),
		nil,
	)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeLocal)}}
	router := chi.NewRouter()
	router.Post("/admin/items/{id}/redetect-intro", handler.HandleRedetectEpisodeIntro)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/items/ep1/redetect-intro", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminIntroRedetectRejectsEpisodeWithoutMediaFiles(t *testing.T) {
	analyzer := &fakeIntroAnalyzer{started: make(chan string, 1), release: make(chan struct{})}
	handler := NewAdminIntroHandler(
		analyzer,
		fakeIntroEligibility{result: &intromarkers.EpisodeIntroEligibility{
			EpisodeID:             "ep1",
			HasMediaFiles:         false,
			IntroDetectionEnabled: false,
		}},
		context.Background(),
		nil,
	)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeLocal)}}
	router := chi.NewRouter()
	router.Post("/admin/items/{id}/redetect-intro", handler.HandleRedetectEpisodeIntro)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/items/ep1/redetect-intro", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case id := <-analyzer.started:
		t.Fatalf("expected analyzer not to start, got %q", id)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAdminIntroRedetectRejectsMissingSettings(t *testing.T) {
	handler := NewAdminIntroHandler(
		&fakeIntroAnalyzer{started: make(chan string, 1), release: make(chan struct{})},
		fakeIntroEligibility{result: &intromarkers.EpisodeIntroEligibility{
			EpisodeID:             "ep1",
			HasMediaFiles:         true,
			IntroDetectionEnabled: true,
		}},
		context.Background(),
		nil,
	)
	router := chi.NewRouter()
	router.Post("/admin/items/{id}/redetect-intro", handler.HandleRedetectEpisodeIntro)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/items/ep1/redetect-intro", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminIntroRedetectNotifiesMarkedFilesAfterAnalyzerSuccess(t *testing.T) {
	start := 12.0
	end := 75.0
	analyzer := &fakeIntroAnalyzer{started: make(chan string, 1)}
	handler := NewAdminIntroHandler(
		analyzer,
		fakeIntroEligibility{result: &intromarkers.EpisodeIntroEligibility{
			EpisodeID:             "ep1",
			HasMediaFiles:         true,
			IntroDetectionEnabled: true,
		}},
		context.Background(),
		nil,
	)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeLocal)}}
	handler.FileResolver = fakeAdminIntroFileResolver{files: []*models.MediaFile{
		{ID: 1, EpisodeID: "ep1", IntroStart: &start, IntroEnd: &end},
		{ID: 2, EpisodeID: "ep1"},
	}}
	notifier := fakeAdminIntroMarkerNotifier{ch: make(chan *models.MediaFile, 1)}
	handler.MarkerUpdateNotifier = notifier
	router := chi.NewRouter()
	router.Post("/admin/items/{id}/redetect-intro", handler.HandleRedetectEpisodeIntro)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/items/ep1/redetect-intro", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected request 202, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case id := <-analyzer.started:
		if id != "ep1" {
			t.Fatalf("expected analyzer to start ep1, got %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("analyzer did not start")
	}

	select {
	case notified := <-notifier.ch:
		if notified.ID != 1 {
			t.Fatalf("notified file ID = %d, want 1", notified.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected marker update notification")
	}
	select {
	case notified := <-notifier.ch:
		t.Fatalf("unexpected second notification: %#v", notified)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestAdminIntroRedetectDoesNotNotifyFilesStillMissingMarkers(t *testing.T) {
	analyzer := &fakeIntroAnalyzer{started: make(chan string, 1)}
	resolverCalled := make(chan string, 1)
	handler := NewAdminIntroHandler(
		analyzer,
		fakeIntroEligibility{result: &intromarkers.EpisodeIntroEligibility{
			EpisodeID:             "ep1",
			HasMediaFiles:         true,
			IntroDetectionEnabled: true,
		}},
		context.Background(),
		nil,
	)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeLocal)}}
	handler.FileResolver = fakeAdminIntroFileResolver{
		files:  []*models.MediaFile{{ID: 1, EpisodeID: "ep1"}},
		called: resolverCalled,
	}
	notifier := fakeAdminIntroMarkerNotifier{ch: make(chan *models.MediaFile, 1)}
	handler.MarkerUpdateNotifier = notifier
	router := chi.NewRouter()
	router.Post("/admin/items/{id}/redetect-intro", handler.HandleRedetectEpisodeIntro)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/items/ep1/redetect-intro", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected request 202, got %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-resolverCalled:
	case <-time.After(time.Second):
		t.Fatal("file resolver was not called")
	}
	select {
	case notified := <-notifier.ch:
		t.Fatalf("unexpected marker update: %#v", notified)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestAdminIntroRedetectNotificationReloadFailureDoesNotFailRequest(t *testing.T) {
	analyzer := &fakeIntroAnalyzer{started: make(chan string, 1)}
	resolverCalled := make(chan string, 1)
	handler := NewAdminIntroHandler(
		analyzer,
		fakeIntroEligibility{result: &intromarkers.EpisodeIntroEligibility{
			EpisodeID:             "ep1",
			HasMediaFiles:         true,
			IntroDetectionEnabled: true,
		}},
		context.Background(),
		nil,
	)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeLocal)}}
	handler.FileResolver = fakeAdminIntroFileResolver{
		err:    errors.New("reload failed"),
		called: resolverCalled,
	}
	handler.MarkerUpdateNotifier = fakeAdminIntroMarkerNotifier{ch: make(chan *models.MediaFile, 1)}
	router := chi.NewRouter()
	router.Post("/admin/items/{id}/redetect-intro", handler.HandleRedetectEpisodeIntro)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/items/ep1/redetect-intro", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected request 202, got %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-resolverCalled:
	case <-time.After(time.Second):
		t.Fatal("file resolver was not called")
	}
}

func decodeRedetectStatus(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var response redetectIntroResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return response.Status
}

// A request for kinds the running analysis does not cover is queued rather
// than dropped: re-detecting markers while an intro-only analysis runs still
// detects credits once it finishes.
func TestAdminEpisodeAnalysisQueuesUncoveredKinds(t *testing.T) {
	analyzer := &fakeIntroAnalyzer{
		started:  make(chan string, 2),
		release:  make(chan struct{}),
		detected: make(chan intromarkers.Detection, 2),
	}
	handler := NewAdminIntroHandler(analyzer, eligibleEpisode(), context.Background(), nil)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeLocal)}}

	if status, err := handler.RefreshEpisodeMarkers(context.Background(), "ep1", "redetect"); err != nil || status != "queued" {
		t.Fatalf("v1 redetect: status=%q err=%v", status, err)
	}
	if got := waitForDetection(t, analyzer); got != (intromarkers.Detection{Intros: true}) {
		t.Fatalf("first run = %+v, want intros", got)
	}
	<-analyzer.started

	if status, err := handler.RefreshEpisodeMarkers(context.Background(), "ep1", "redetect"); err != nil || status != "already_running" {
		t.Fatalf("covered request: status=%q err=%v, want already_running", status, err)
	}
	if status, err := handler.RefreshEpisodeMarkers(context.Background(), "ep1", "redetect-v2"); err != nil || status != "queued" {
		t.Fatalf("uncovered request: status=%q err=%v, want queued", status, err)
	}
	if status, err := handler.RefreshEpisodeMarkers(context.Background(), "ep1", "redetect-v2"); err != nil || status != "already_running" {
		t.Fatalf("request covered by the queue: status=%q err=%v, want already_running", status, err)
	}

	analyzer.release <- struct{}{}
	if got := waitForDetection(t, analyzer); got != (intromarkers.Detection{Credits: true}) {
		t.Fatalf("queued run = %+v, want the credits the first run did not cover", got)
	}
	<-analyzer.started
	analyzer.release <- struct{}{}
}

// In on-demand mode an explicit refresh must not drop the unsaved online
// intro from the update it sends after local detection.
func TestAdminMarkerRefreshV2NotificationKeepsOnlineMarkers(t *testing.T) {
	scanner, online := models.MarkerSourceScanner, models.MarkerSourceOnline
	creditsStart, creditsEnd, introStart, introEnd := 1700.0, 1760.0, 20.0, 80.0
	stored := &models.MediaFile{ID: 42, EpisodeID: "ep1", CreditsStart: &creditsStart, CreditsEnd: &creditsEnd, CreditsMarkersSource: &scanner}
	analyzer := &fakeIntroAnalyzer{detected: make(chan intromarkers.Detection, 1)}
	notifier := fakeAdminIntroMarkerNotifier{ch: make(chan *models.MediaFile, 1)}
	handler := NewAdminIntroHandler(analyzer, eligibleEpisode(), context.Background(), nil)
	handler.Settings = fakeMarkerSettings{values: map[string]string{markers.SettingMode: string(markers.ModeBoth)}}
	handler.FileResolver = fakeAdminIntroFileResolver{files: []*models.MediaFile{stored}}
	handler.MarkerUpdateNotifier = notifier
	handler.OnlineMarkers = markerRefreshFunc(func(_ context.Context, file *models.MediaFile) (*models.MediaFile, bool, error) {
		overlay := *file
		overlay.IntroStart, overlay.IntroEnd, overlay.IntroMarkersSource = &introStart, &introEnd, &online
		return &overlay, true, nil
	})
	if status, err := handler.RefreshEpisodeMarkers(context.Background(), "ep1", "refresh-v2"); err != nil || status != "queued" {
		t.Fatalf("refresh: status=%q err=%v", status, err)
	}
	if got := waitForDetection(t, analyzer); got != (intromarkers.Detection{Credits: true}) {
		t.Fatalf("detection = %+v, want the detector's own credits", got)
	}
	select {
	case sent := <-notifier.ch:
		if sent.IntroStart == nil || *sent.IntroStart != introStart {
			t.Fatalf("update dropped the online intro: %+v", sent)
		}
		if sent.CreditsStart == nil || *sent.CreditsStart != creditsStart {
			t.Fatalf("update lacks the stored credits: %+v", sent)
		}
	case <-time.After(time.Second):
		t.Fatal("no marker update was sent")
	}
}

// Refreshing online markers does not depend on the detection settings, so a
// failure to read them only skips local analysis.
func TestAdminMarkerRefreshV2SettingsFailureStillRefreshesOnline(t *testing.T) {
	refreshed := make(chan int, 1)
	settings := failingDetectionSettings{mode: string(markers.ModeBoth)}
	handler := NewAdminIntroHandler(&fakeIntroAnalyzer{}, eligibleEpisode(), context.Background(), nil)
	handler.Settings = settings
	handler.FileResolver = fakeAdminIntroFileResolver{files: []*models.MediaFile{{ID: 42, EpisodeID: "ep1"}}}
	handler.OnlineMarkers = markerRefreshFunc(func(_ context.Context, file *models.MediaFile) (*models.MediaFile, bool, error) {
		refreshed <- file.ID
		return file, false, nil
	})
	if status, err := handler.RefreshEpisodeMarkers(context.Background(), "ep1", "refresh-v2"); err != nil || status != "queued" {
		t.Fatalf("refresh: status=%q err=%v, want the online refresh queued", status, err)
	}
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("online refresh did not run")
	}
}

type failingDetectionSettings struct{ mode string }

func (s failingDetectionSettings) Get(_ context.Context, key string) (string, error) {
	if key == markers.SettingMode {
		return s.mode, nil
	}
	return "", errors.New("settings unavailable")
}
