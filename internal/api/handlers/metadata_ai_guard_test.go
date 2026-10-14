package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/metadata/translation"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/ratelimit"
)

type guardItemAccess struct{}

func (guardItemAccess) GetByID(_ context.Context, contentID string) (*models.MediaItem, error) {
	return &models.MediaItem{ContentID: contentID, Overview: "An overview."}, nil
}

func (guardItemAccess) EnsureAccessible(context.Context, string, catalog.AccessFilter) error {
	return nil
}

type guardPending struct{ language string }

func (p guardPending) PendingTranslationLanguage(context.Context, *models.MediaItem, catalog.AccessFilter) string {
	return p.language
}

func (p guardPending) PendingSeasonTranslationLanguage(context.Context, *models.Season, catalog.AccessFilter) string {
	return p.language
}

func (p guardPending) PendingEpisodeTranslationLanguage(context.Context, *models.Episode, catalog.AccessFilter) string {
	return p.language
}

type denyLimiter struct{}

func (denyLimiter) Allow(context.Context, string, ratelimit.Rate) ratelimit.AllowResult {
	return ratelimit.AllowResult{Allowed: false}
}
func (denyLimiter) Close() {}

// guardJobs is the translation job store behind the guard tests. active is
// the job a retry coalesces onto; RequestOnView's first read fails with
// errGuardPassed, so a request that passes every guard ends in a 500.
type guardJobs struct {
	translation.JobRepository
	active *translation.Job
}

var errGuardPassed = errors.New("passed every guard")

func (j *guardJobs) GetActiveJobByIdempotencyKey(context.Context, string) (*translation.Job, error) {
	return j.active, nil
}

func (j *guardJobs) ListJobsByContent(context.Context, string) ([]translation.Job, error) {
	return nil, errGuardPassed
}

// passedGuards reports whether err is the answer to a request that passed
// every guard and reached the service.
func passedGuards(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusInternalServerError
}

var guardConfig = translation.Config{Enabled: true, Configured: true, ChatModel: "test-model", OnView: "auto"}

// newGuardHandler wires a handler whose on-view translation is on and whose
// job store has no active job.
func newGuardHandler(pending string, limiter ratelimit.RateLimiter) *MetadataAIHandler {
	return newGuardHandlerWith(guardConfig, &guardJobs{}, pending, limiter)
}

func newGuardHandlerWith(cfg translation.Config, jobs *guardJobs, pending string, limiter ratelimit.RateLimiter) *MetadataAIHandler {
	svc := translation.NewService(context.Background(), cfg, jobs, nil, nil, nil, nil, nil)
	h := NewMetadataAIHandler(svc)
	h.ItemAccess = guardItemAccess{}
	h.Pending = guardPending{language: pending}
	h.Limiter = limiter
	return h
}

func TestTranslateOnViewOnlyQueuesTheViewersPendingLanguage(t *testing.T) {
	userID := 7
	cases := []struct {
		name       string
		pending    string
		requested  string
		limiter    ratelimit.RateLimiter
		wantStatus int
		wantField  string
	}{
		{name: "nothing missing", pending: "", requested: "de", wantStatus: http.StatusBadRequest, wantField: "target_language"},
		{name: "another language", pending: "de", requested: "ja", wantStatus: http.StatusBadRequest, wantField: "target_language"},
		{name: "rate limited", pending: "de", requested: "de", limiter: denyLimiter{}, wantStatus: http.StatusTooManyRequests},
		{name: "matching language reaches the service", pending: "de", requested: "DE", wantStatus: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGuardHandler(tc.pending, tc.limiter)
			_, err := h.TranslateOnView(context.Background(), catalog.AccessFilter{}, "item1", tc.requested, &userID)
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.Status != tc.wantStatus || apiErr.Field != tc.wantField {
				t.Fatalf("got status %d field %q (%s), want %d field %q", apiErr.Status, apiErr.Field, apiErr.Message, tc.wantStatus, tc.wantField)
			}
		})
	}
}

// libraryPending reports German only for a page scoped to library 7, as a
// detail page does for a profile without a metadata language.
type libraryPending struct{}

func (libraryPending) PendingTranslationLanguage(_ context.Context, _ *models.MediaItem, filter catalog.AccessFilter) string {
	if filter.PresentationLibraryID != nil && *filter.PresentationLibraryID == 7 {
		return "de"
	}
	return ""
}

func (libraryPending) PendingSeasonTranslationLanguage(context.Context, *models.Season, catalog.AccessFilter) string {
	return ""
}

func (libraryPending) PendingEpisodeTranslationLanguage(context.Context, *models.Episode, catalog.AccessFilter) string {
	return ""
}

type itemLibraries []int

func (l itemLibraries) LibraryIDsForItem(context.Context, string) ([]int, error) { return l, nil }

func TestTranslateOnViewAcceptsTheLanguageOfAViewerLibrary(t *testing.T) {
	userID := 7
	h := newGuardHandler("", nil)
	h.Pending = libraryPending{}
	h.ItemLibraries = itemLibraries{3, 7}

	// The library-scoped page asked for German: the check reaches the service.
	_, err := h.TranslateOnView(context.Background(), catalog.AccessFilter{}, "item1", "de", &userID)
	if !passedGuards(err) {
		t.Fatalf("library language: err = %v, want it to reach the service", err)
	}
	var apiErr *APIError
	// A library the viewer cannot see does not count.
	_, err = h.TranslateOnView(context.Background(), catalog.AccessFilter{DisabledLibraryIDs: []int{7}}, "item1", "de", &userID)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("hidden library: err = %v, want 400", err)
	}
	_, err = h.TranslateOnView(context.Background(), catalog.AccessFilter{AllowedLibraryIDs: []int{3}}, "item1", "de", &userID)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("library outside the allowlist: err = %v, want 400", err)
	}
	// Other languages are still refused.
	_, err = h.TranslateOnView(context.Background(), catalog.AccessFilter{}, "item1", "ja", &userID)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("other language: err = %v, want 400", err)
	}
}

// episodeOnlyAccess resolves every ID as an episode of series1.
type episodeOnlyAccess struct{}

func (episodeOnlyAccess) GetByID(context.Context, string) (*models.MediaItem, error) {
	return nil, catalog.ErrItemNotFound
}

func (episodeOnlyAccess) EnsureAccessible(context.Context, string, catalog.AccessFilter) error {
	return nil
}

type episodeLookup struct{}

func (episodeLookup) GetByID(_ context.Context, contentID string) (*models.Episode, error) {
	return &models.Episode{ContentID: contentID, SeriesID: "series1", Overview: "An overview."}, nil
}

// episodeLibraryPending reports German for an episode page scoped to library 7.
type episodeLibraryPending struct{ libraryPending }

func (episodeLibraryPending) PendingEpisodeTranslationLanguage(_ context.Context, _ *models.Episode, filter catalog.AccessFilter) string {
	if filter.PresentationLibraryID != nil && *filter.PresentationLibraryID == 7 {
		return "de"
	}
	return ""
}

// membershipsByID answers the libraries of each content ID.
type membershipsByID map[string][]int

func (m membershipsByID) LibraryIDsForItem(_ context.Context, contentID string) ([]int, error) {
	return m[contentID], nil
}

// An episode page is scoped by the episode's own libraries, not its series'.
func TestTranslateOnViewUsesTheEpisodesOwnLibraries(t *testing.T) {
	userID := 7
	h := newGuardHandler("", nil)
	h.ItemAccess = episodeOnlyAccess{}
	h.EpisodeLookup = episodeLookup{}
	h.Pending = episodeLibraryPending{}

	h.ItemLibraries = membershipsByID{"series1": {3}, "ep1": {7}}
	_, err := h.TranslateOnView(context.Background(), catalog.AccessFilter{}, "ep1", "de", &userID)
	if !passedGuards(err) {
		t.Fatalf("episode library: err = %v, want it to reach the service", err)
	}
	var apiErr *APIError

	h.ItemLibraries = membershipsByID{"series1": {3, 7}, "ep1": {3}}
	_, err = h.TranslateOnView(context.Background(), catalog.AccessFilter{}, "ep1", "de", &userID)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("series-only library: err = %v, want 400", err)
	}

	// An episode in no library is not widened to the viewer's allowlist.
	h.ItemLibraries = membershipsByID{"series1": {7}}
	_, err = h.TranslateOnView(context.Background(), catalog.AccessFilter{AllowedLibraryIDs: []int{7}}, "ep1", "de", &userID)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("episode without libraries: err = %v, want 400", err)
	}
}

// failOpenLimiter is a shared limiter whose store is unreachable: it allows
// everything and says so with Remaining -1.
type failOpenLimiter struct{}

func (failOpenLimiter) Allow(context.Context, string, ratelimit.Rate) ratelimit.AllowResult {
	return ratelimit.AllowResult{Allowed: true, Remaining: -1}
}
func (failOpenLimiter) Close() {}

func TestTranslateOnViewKeepsABudgetWhileTheSharedLimiterFailsOpen(t *testing.T) {
	userID := 7
	h := newGuardHandler("de", failOpenLimiter{})
	var limited int
	for i := 0; i < 15; i++ {
		_, err := h.TranslateOnView(context.Background(), catalog.AccessFilter{}, "item1", "de", &userID)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("no request was limited while the shared limiter failed open")
	}
}

// A refused request spends budget before the pending-language lookup runs.
func TestTranslateOnViewChecksTheBudgetBeforeTheLanguage(t *testing.T) {
	userID := 7
	h := newGuardHandler("de", denyLimiter{})
	_, err := h.TranslateOnView(context.Background(), catalog.AccessFilter{}, "item1", "ja", &userID)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want 429 before the language check", err)
	}
}

type waitLimiter struct{ wait time.Duration }

func (l waitLimiter) Allow(context.Context, string, ratelimit.Rate) ratelimit.AllowResult {
	return ratelimit.AllowResult{Allowed: false, RetryAfter: l.wait}
}
func (waitLimiter) Close() {}

// Retry-After rounds up, so a client that waits the advertised time is let in.
func TestTranslateOnViewRoundsRetryAfterUp(t *testing.T) {
	userID := 7
	h := newGuardHandler("de", waitLimiter{wait: 1500 * time.Millisecond})
	_, err := h.TranslateOnView(context.Background(), catalog.AccessFilter{}, "item1", "de", &userID)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != 2 {
		t.Fatalf("err = %#v, want a 429 with Retry-After 2", err)
	}
}

// countingLimiter allows everything and counts the requests that spent budget.
type countingLimiter struct{ calls int }

func (l *countingLimiter) Allow(context.Context, string, ratelimit.Rate) ratelimit.AllowResult {
	l.calls++
	return ratelimit.AllowResult{Allowed: true, Remaining: 1}
}
func (*countingLimiter) Close() {}

// With on-view translation off, every request gets the not-configured answer
// without spending budget, whatever its language.
func TestTranslateOnViewAnswersNotConfiguredBeforeTheGuards(t *testing.T) {
	userID := 7
	limiter := &countingLimiter{}
	off := guardConfig
	off.OnView = "off"
	h := newGuardHandlerWith(off, &guardJobs{}, "", limiter)
	_, err := h.TranslateOnView(context.Background(), catalog.AccessFilter{}, "item1", "ja", &userID)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != "not_configured" {
		t.Fatalf("err = %v, want 503 not_configured", err)
	}
	if limiter.calls != 0 {
		t.Fatalf("limiter calls = %d, want none while the feature is off", limiter.calls)
	}
}

// A retry after a lost response gets the running job back even once the job
// has filled the description and nothing is reported missing any more.
func TestTranslateOnViewReturnsTheActiveJobToARetry(t *testing.T) {
	userID := 7
	active := &translation.Job{ID: 9, TargetKind: translation.TargetItem, ContentID: "item1", TargetLanguage: "de"}
	h := newGuardHandlerWith(guardConfig, &guardJobs{active: active}, "", nil)
	job, err := h.TranslateOnView(context.Background(), catalog.AccessFilter{}, "item1", "de", &userID)
	if err != nil || job == nil || job.ID != 9 {
		t.Fatalf("job = %#v, err = %v, want the active job 9", job, err)
	}
}
