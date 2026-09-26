package handlers

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/intromarkers"
	"github.com/Silo-Server/silo-server/internal/markers"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

const playbackLazyMarkerTimeout = 10 * time.Minute

type PlaybackIntroEligibilityChecker interface {
	IntroDetectionEligibleForPlayback(ctx context.Context, fileID int) (bool, error)
	IsFileInEnabledLibrary(ctx context.Context, fileID int) (bool, error)
}

type PlaybackMarkerUpdateNotifier interface {
	MarkersUpdated(ctx context.Context, file *models.MediaFile)
}

func (h *PlaybackHandler) maybeQueueLazyPlaybackMarkers(
	ctx context.Context,
	session *playback.Session,
	file *models.MediaFile,
) {
	if h == nil || session == nil || file == nil || file.ID <= 0 {
		return
	}
	if file.MediaFolderID <= 0 {
		return
	}
	isEpisode := strings.TrimSpace(file.EpisodeID) != ""
	isMovie := !isEpisode && strings.TrimSpace(file.ContentID) != ""
	if !isEpisode && !isMovie {
		return
	}
	if h.SettingsRepo == nil || h.IntroRepository == nil {
		return
	}

	lazy, err := h.SettingsRepo.Get(ctx, markers.SettingLazyPlayback)
	if err != nil {
		slog.WarnContext(ctx, "playback lazy markers: load lazy setting failed", "component", "api",
			"session_id", session.ID,
			"file_id", file.ID,
			"episode_id", file.EpisodeID,
			"error", err)
		return
	}

	rawMode, err := h.SettingsRepo.Get(ctx, markers.SettingMode)
	if err != nil {
		slog.WarnContext(ctx, "playback lazy markers: load marker mode failed", "component", "api",
			"session_id", session.ID,
			"file_id", file.ID,
			"episode_id", file.EpisodeID,
			"error", err)
		return
	}
	mode := markers.NormalizeMode(rawMode)
	lazyEnabled := strings.EqualFold(strings.TrimSpace(lazy), "true")
	if !lazyEnabled {
		storage, err := h.SettingsRepo.Get(ctx, markers.SettingOnlineStorage)
		if err != nil || storage != "on_demand" || (mode != markers.ModeOnline && mode != markers.ModeBoth) {
			return
		}
	}
	if mode == markers.ModeOff {
		slog.DebugContext(ctx, "playback lazy markers: skipped; marker mode is off", "component", "api",
			"session_id", session.ID,
			"file_id", file.ID,
			"episode_id", file.EpisodeID)
		return
	}

	hasOnline := h.hasOnlineMarkerProviders()
	shouldRunLocal := lazyEnabled && markers.ShouldRunLocal(mode)
	shouldRunOnline := (mode == markers.ModeOnline || mode == markers.ModeBoth) && hasOnline

	if shouldRunOnline {
		// Online providers work for any enabled library (movies and series alike).
		ok, err := h.IntroRepository.IsFileInEnabledLibrary(ctx, file.ID)
		if err != nil {
			slog.WarnContext(ctx, "playback lazy markers: online eligibility check failed", "component", "api",
				"session_id", session.ID,
				"file_id", file.ID,
				"error", err)
			shouldRunOnline = false
		}
		if !ok {
			shouldRunOnline = false
		}
	}

	if shouldRunLocal {
		// Local chromaprint is only meaningful for series libraries that
		// opted in to expensive fingerprinting and requires an analyzer.
		ok, err := h.IntroRepository.IntroDetectionEligibleForPlayback(ctx, file.ID)
		if err != nil {
			slog.WarnContext(ctx, "playback lazy markers: local eligibility check failed", "component", "api",
				"session_id", session.ID,
				"file_id", file.ID,
				"episode_id", file.EpisodeID,
				"mode", mode,
				"error", err)
			shouldRunLocal = false
		}
		if !ok || h.IntroAnalyzer == nil || !isEpisode {
			shouldRunLocal = false
		}
	}

	if !shouldRunOnline && !shouldRunLocal {
		slog.DebugContext(ctx, "playback lazy markers: skipped; no eligible detection path", "component", "api",
			"session_id", session.ID,
			"file_id", file.ID,
			"episode_id", file.EpisodeID,
			"mode", mode)
		return
	}

	if _, loaded := h.MarkerLazyInFlight.LoadOrStore(file.ID, struct{}{}); loaded {
		return
	}

	sessionID := session.ID
	fileSnapshot := *file
	slog.InfoContext(ctx, "playback lazy markers: queued", "component", "api",
		"session_id", sessionID,
		"file_id", file.ID,
		"episode_id", file.EpisodeID,
		"mode", mode,
		"run_online", shouldRunOnline,
		"run_local", shouldRunLocal)
	go h.runLazyPlaybackMarkers(sessionID, &fileSnapshot, mode, shouldRunOnline, shouldRunLocal)
}

func (h *PlaybackHandler) runLazyPlaybackMarkers(
	sessionID string,
	file *models.MediaFile,
	mode markers.Mode,
	runOnline bool,
	runLocal bool,
) {
	if file == nil {
		return
	}
	defer h.MarkerLazyInFlight.Delete(file.ID)

	base := h.MarkerLazyContext
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, playbackLazyMarkerTimeout)
	defer cancel()

	slog.Info("playback lazy markers: started",
		"session_id", sessionID,
		"file_id", file.ID,
		"episode_id", file.EpisodeID,
		"mode", mode)

	// The detection settings are read here, off the playback-start path.
	var detect intromarkers.Detection
	if runLocal {
		var err error
		if detect, err = intromarkers.LoadDetection(ctx, h.SettingsRepo); err != nil {
			slog.WarnContext(ctx, "playback lazy markers: load detection settings failed", "component", "api",
				"session_id", sessionID,
				"file_id", file.ID,
				"error", err)
			runLocal = false
		}
	}

	// current is the file as players should see it: stored markers plus any
	// on-demand online markers, which are never saved.
	current := h.playbackMarkerView(ctx, file, runOnline)
	if hasAnyMarker(current) {
		h.notifyPlaybackMarkers(ctx, sessionID, current, mode)
	}
	// Online markers take priority, so only the kinds still missing are
	// detected here, and only kinds not already tried for this file recently.
	missing := h.markerLazyAttempts.take(file.ID, detect.Missing(current), time.Now())
	if !runLocal || !missing.Any() {
		return
	}

	slog.Info("playback lazy markers: local analyzer started",
		"session_id", sessionID,
		"file_id", file.ID,
		"episode_id", file.EpisodeID,
		"mode", mode,
		"detect_intros", missing.Intros,
		"detect_credits", missing.Credits)
	// A viewer is waiting: take the ffmpeg slot reserved for playback.
	summary, err := h.IntroAnalyzer.AnalyzeEpisode(intromarkers.WithPlaybackPriority(ctx), file.EpisodeID,
		missing)
	if err != nil {
		slog.Warn("playback lazy markers: local analyzer failed",
			"session_id", sessionID,
			"file_id", file.ID,
			"episode_id", file.EpisodeID,
			"mode", mode,
			"error", err)
		return
	}
	slog.Info("playback lazy markers: local analyzer finished",
		"session_id", sessionID,
		"file_id", file.ID,
		"episode_id", file.EpisodeID,
		"mode", mode,
		"files_considered", summary.FilesConsidered,
		"season_groups_considered", summary.SeasonGroupsConsidered,
		"chapter_markers_written", summary.ChapterMarkersWritten,
		"chromaprint_markers_written", summary.ChromaprintMarkersWritten,
		"credits_chapter_markers_written", summary.CreditsChapterMarkersWritten,
		"credits_chromaprint_markers_written", summary.CreditsChromaprintWritten,
		"fingerprint_cache_hits", summary.FingerprintCacheHits,
		"fingerprints_computed", summary.FingerprintsComputed,
		"errors", len(summary.Errors))

	// The stored file now carries the detected markers; on-demand online
	// markers from the earlier view are laid over it without a second lookup.
	if after := markers.OverlayOnline(h.reloadPlaybackMarkerFile(ctx, file.ID), current); hasAnyMarker(after) {
		h.notifyPlaybackMarkers(ctx, sessionID, after, mode)
	}
}

// lazyDetectionRetryInterval bounds how often playback re-runs local detection
// of a kind for a file it already tried. A season comparison that found no
// marker finds none on the next start either, and each attempt compares the
// whole season on the ffmpeg slot reserved for playback.
const lazyDetectionRetryInterval = 6 * time.Hour

// lazyDetectionLog records when playback last ran local detection of each kind
// per file. It is process-local: each node may try once per interval.
type lazyDetectionLog struct {
	mu    sync.Mutex
	tried map[int]lazyDetectionAttempt
}

type lazyDetectionAttempt struct {
	intros, credits time.Time
}

// maxLazyDetectionEntries bounds the log; older entries are pruned first.
const maxLazyDetectionEntries = 10000

// take returns the kinds of detect not tried for fileID within the retry
// interval, and records them as tried at now.
func (l *lazyDetectionLog) take(fileID int, detect intromarkers.Detection, now time.Time) intromarkers.Detection {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tried == nil {
		l.tried = map[int]lazyDetectionAttempt{}
	}
	attempt := l.tried[fileID]
	due := intromarkers.Detection{
		Intros:  detect.Intros && now.Sub(attempt.intros) >= lazyDetectionRetryInterval,
		Credits: detect.Credits && now.Sub(attempt.credits) >= lazyDetectionRetryInterval,
	}
	if !due.Any() {
		return due
	}
	if due.Intros {
		attempt.intros = now
	}
	if due.Credits {
		attempt.credits = now
	}
	if len(l.tried) >= maxLazyDetectionEntries {
		for id, old := range l.tried {
			if now.Sub(old.intros) >= lazyDetectionRetryInterval && now.Sub(old.credits) >= lazyDetectionRetryInterval {
				delete(l.tried, id)
			}
		}
	}
	l.tried[fileID] = attempt
	return due
}

// playbackMarkerView returns the file with the markers players should see.
// Online lookup reloads the stored row itself and, in on-demand mode, overlays
// markers that are never saved, so its result must not be replaced by the
// stored row. Otherwise the stored row is reloaded, because a concurrent
// session may have populated markers since this run was queued.
func (h *PlaybackHandler) playbackMarkerView(ctx context.Context, file *models.MediaFile, runOnline bool) *models.MediaFile {
	if runOnline {
		effective, current, err := h.MarkerPopulation.Populate(ctx, file)
		if err != nil {
			slog.WarnContext(ctx, "playback marker lookup failed", "file_id", file.ID, "error", err)
		}
		if current && effective != nil {
			return effective
		}
	}
	if refreshed := h.reloadPlaybackMarkerFile(ctx, file.ID); refreshed != nil {
		return refreshed
	}
	return file
}

func (h *PlaybackHandler) hasOnlineMarkerProviders() bool {
	return h != nil && h.MarkerPopulation != nil && h.MarkerRegistry != nil && len(h.MarkerRegistry.Providers()) > 0
}

func (h *PlaybackHandler) reloadPlaybackMarkerFile(ctx context.Context, fileID int) *models.MediaFile {
	if h == nil || h.fileResolver == nil || fileID <= 0 {
		return nil
	}
	refreshed, err := h.fileResolver.GetByID(ctx, fileID)
	if err != nil {
		slog.WarnContext(ctx, "playback lazy markers: reload file failed", "component", "api", "file_id", fileID, "error", err)
		return nil
	}
	return refreshed
}

func (h *PlaybackHandler) notifyPlaybackMarkers(
	ctx context.Context,
	sessionID string,
	file *models.MediaFile,
	mode markers.Mode,
) {
	if h == nil || h.MarkerUpdateNotifier == nil || file == nil {
		return
	}
	h.MarkerUpdateNotifier.MarkersUpdated(ctx, file)
	slog.InfoContext(ctx, "playback lazy markers: emitted marker update", "component", "api",
		"session_id", sessionID,
		"file_id", file.ID,
		"episode_id", file.EpisodeID,
		"mode", mode)
}

// hasAnyMarker reports whether the file has at least one populated marker
// segment. Used to decide whether to emit a markers_updated event.
func hasAnyMarker(file *models.MediaFile) bool {
	if file == nil {
		return false
	}
	return (file.IntroStart != nil && file.IntroEnd != nil) ||
		(file.CreditsStart != nil && file.CreditsEnd != nil) ||
		(file.RecapStart != nil && file.RecapEnd != nil) ||
		(file.PreviewStart != nil && file.PreviewEnd != nil)
}
