package handlers

import (
	"context"
	"net/http"

	"github.com/Silo-Server/silo-server/internal/intromarkers"
	"github.com/Silo-Server/silo-server/internal/markers"
	"github.com/Silo-Server/silo-server/internal/models"
)

type MarkerRefreshService interface {
	Refresh(context.Context, *models.MediaFile) (*models.MediaFile, bool, error)
}

const (
	markerRefreshQueued         = "queued"
	markerRefreshAlreadyRunning = "already_running"
)

func (h *AdminIntroHandler) refreshEpisodeMarkersV2(ctx context.Context, episodeID string) (string, error) {
	if h == nil || h.Settings == nil || h.FileResolver == nil {
		return "", apiError(http.StatusServiceUnavailable, "unavailable", "Marker refresh is not configured")
	}
	raw, err := h.Settings.Get(ctx, markers.SettingMode)
	if err != nil {
		return "", apiError(http.StatusInternalServerError, "internal_error", "Failed to load marker settings")
	}
	mode := markers.NormalizeMode(raw)
	if mode == markers.ModeOff {
		return "", apiError(http.StatusConflict, "conflict", "Marker detection is disabled")
	}
	if mode == markers.ModeLocal {
		return h.analyzeEpisodeLocally(ctx, episodeID, "refresh", true)
	}
	if h.OnlineMarkers == nil {
		return "", apiError(http.StatusServiceUnavailable, "unavailable", "Online markers are not configured")
	}
	files, err := h.FileResolver.GetByEpisodeID(ctx, episodeID)
	if err != nil {
		return "", apiError(http.StatusInternalServerError, "internal_error", "Failed to resolve episode files")
	}
	if len(files) == 0 {
		return "", apiError(http.StatusConflict, "conflict", "Episode has no media files to refresh")
	}
	local := false
	var detect intromarkers.Detection
	if mode == markers.ModeBoth && h.analyzer != nil && h.eligibility != nil {
		eligibility, err := h.eligibility.EpisodeIntroEligibility(ctx, episodeID)
		if err != nil {
			return "", apiError(http.StatusInternalServerError, "internal_error", "Failed to resolve marker eligibility")
		}
		if eligibility.IntroDetectionEnabled {
			// The online refresh does not depend on the detection settings,
			// so failing to read them only skips local analysis.
			if detect, err = intromarkers.LoadDetection(ctx, h.Settings); err != nil {
				h.logger.WarnContext(ctx, "admin markers: load detection settings failed; skipping local analysis",
					"episode_id", episodeID, "error", err)
			}
			local = err == nil && detect.Any()
		}
	}
	if _, loaded := h.inFlight.LoadOrStore(episodeID, struct{}{}); loaded {
		return markerRefreshAlreadyRunning, nil
	}
	go func() {
		defer h.inFlight.Delete(episodeID)
		ctx, cancel := context.WithTimeout(h.baseContext, playbackLazyMarkerTimeout)
		defer cancel()
		// Local detection re-runs every selected kind that online providers
		// and editors did not supply, including markers it wrote earlier.
		var redetect intromarkers.Detection
		// views keeps each file's refreshed markers, which in on-demand mode
		// include online markers that are never saved.
		views := make(map[int]*models.MediaFile, len(files))
		for _, file := range files {
			if file == nil || ctx.Err() != nil {
				continue
			}
			effective, _, err := h.OnlineMarkers.Refresh(ctx, file)
			if err != nil {
				h.logger.WarnContext(ctx, "online marker refresh failed", "file_id", file.ID, "error", err)
			}
			if effective != nil {
				views[file.ID] = effective
			}
			redetect = redetect.Union(detect.Redetectable(effective))
		}
		if local && redetect.Any() && ctx.Err() == nil {
			if _, err := h.analyzer.AnalyzeEpisode(ctx, episodeID, redetect); err != nil {
				h.logger.WarnContext(ctx, "local marker refresh failed", "episode_id", episodeID, "error", err)
			}
			h.notifyEpisodeMarkerUpdates(ctx, episodeID, "refresh", views)
		}
	}()
	return markerRefreshQueued, nil
}
