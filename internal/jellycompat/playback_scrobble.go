package jellycompat

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/userstore"
	"github.com/Silo-Server/silo-server/internal/watchsync"
)

type compatScrobbleAction string

const (
	compatScrobbleStart compatScrobbleAction = "start"
	compatScrobblePause compatScrobbleAction = "pause"
	compatScrobbleStop  compatScrobbleAction = "stop"
)

const (
	// compatResumeScrobbleTolerance is how far a report may drift from where
	// the start scrobble places playback before the start is re-sent. It
	// exceeds startup buffering and report jitter, so ordinary play from the
	// start or from StartTimeTicks sends a single start.
	compatResumeScrobbleTolerance = 30.0
	// compatResumeScrobbleWindow bounds how long after a start a report may
	// still correct it. Clients seek to their resume point as playback
	// begins; a later jump is an ordinary seek, which scrobbles only on the
	// next pause or stop, as on the native API.
	compatResumeScrobbleWindow = 2 * time.Minute
)

func (h *PlaybackHandler) dispatchCompatScrobble(
	ctx context.Context,
	action compatScrobbleAction,
	playSession *PlaybackSession,
	upstreamSession *playback.Session,
	preferredSource *PlaybackMediaSource,
) error {
	return h.dispatchCompatScrobbleAt(ctx, action, playSession, upstreamSession, preferredSource, nil)
}

func (h *PlaybackHandler) dispatchCompatScrobbleAt(
	ctx context.Context,
	action compatScrobbleAction,
	playSession *PlaybackSession,
	upstreamSession *playback.Session,
	preferredSource *PlaybackMediaSource,
	positionOverride *float64,
) error {
	event, ok := h.compatScrobbleEvent(
		ctx, action, playSession, upstreamSession, preferredSource, positionOverride,
	)
	if !ok {
		return nil
	}
	return h.dispatchCompatScrobbleEvent(ctx, action, event)
}

func (h *PlaybackHandler) compatScrobbleEvent(
	ctx context.Context,
	action compatScrobbleAction,
	playSession *PlaybackSession,
	upstreamSession *playback.Session,
	preferredSource *PlaybackMediaSource,
	positionOverride *float64,
) (watchsync.ScrobbleEvent, bool) {
	if h == nil || h.WatchScrobbler == nil || playSession == nil || upstreamSession == nil ||
		upstreamSession.DisableProgressPersistence || playSession.ItemID == "" {
		return watchsync.ScrobbleEvent{}, false
	}
	scrobbleCtx, cancel := compatDetachedContext(ctx)
	defer cancel()

	source := compatScrobbleSource(playSession, upstreamSession, preferredSource)
	duration := 0.0
	if source != nil {
		duration = float64(source.Version.Duration)
	}
	position := compatScrobblePosition(playSession, upstreamSession)
	if positionOverride != nil {
		position = *positionOverride
	}
	completed := false
	if action == compatScrobbleStop && duration > 0 {
		_, completed, _ = userstore.ResolveProgressState(position, duration, h.playbackThresholds(scrobbleCtx))
	}
	event := watchsync.ResolveScrobbleIdentity(scrobbleCtx, h.StableIdentityResolver, watchsync.ScrobbleEvent{
		PlaybackSessionID: upstreamSession.ID,
		UserID:            upstreamSession.UserID,
		ProfileID:         upstreamSession.ProfileID,
		MediaItemID:       playSession.ItemID,
		PositionSeconds:   position,
		DurationSeconds:   duration,
		Completed:         completed,
		OccurredAt:        time.Now().UTC(),
	})
	return event, true
}

// compatScrobblePosition is the position a scrobble reports when the caller
// has no fresher sample: the upstream position, or the StartTimeTicks seek
// before the first report moves it.
func compatScrobblePosition(playSession *PlaybackSession, upstreamSession *playback.Session) float64 {
	position := upstreamSession.Position
	if position <= 0 && playSession.InitialSeekSeconds > 0 {
		position = playSession.InitialSeekSeconds
	}
	return position
}

// recordCompatResumeScrobble remembers the start just sent for a new or
// rebuilt upstream session so a client report can still correct its position.
// Some clients resume through PositionTicks on their first Playing report
// instead of StartTimeTicks on PlaybackInfo, and a session revived mid-play
// starts again from zero.
func (h *PlaybackHandler) recordCompatResumeScrobble(playSession *PlaybackSession, upstreamSession *playback.Session) {
	if h == nil || h.playbackStore == nil || playSession == nil || upstreamSession == nil {
		return
	}
	upstreamID := upstreamSession.ID
	position := compatScrobblePosition(playSession, upstreamSession)
	sentAt := time.Now()
	// The durable store may replay this callback from another request after a
	// failed write, so it only reads captured values and writes the session;
	// a mismatch is skipped rather than failed, since a replay error would fail
	// every later durable write for the session.
	_ = h.playbackStore.Update(playSession.ID, func(current *PlaybackSession) error {
		if current.UpstreamSessionID == upstreamID {
			current.ResumeScrobbleUpstreamID = upstreamID
			current.ResumeScrobblePosition = position
			current.ResumeScrobbleSentAt = sentAt
		}
		return nil
	})
	if stored, ok := h.playbackStore.Get(playSession.ID); ok &&
		stored.ResumeScrobbleUpstreamID == upstreamID && stored.ResumeScrobbleSentAt.Equal(sentAt) {
		playSession.ResumeScrobbleUpstreamID = upstreamID
		playSession.ResumeScrobblePosition = position
		playSession.ResumeScrobbleSentAt = sentAt
	}
}

// clearCompatResumeScrobble ends the correction window once the provider has
// an accurate position from a correction, pause, or resume. It clears only the
// record the report saw, so a stale report cannot end the window of a start
// that a concurrent stream request sent for a replacement upstream session.
// Like recordCompatResumeScrobble, the callback is replay-safe.
func (h *PlaybackHandler) clearCompatResumeScrobble(playSession *PlaybackSession) {
	if h == nil || h.playbackStore == nil || playSession == nil || playSession.ResumeScrobbleUpstreamID == "" {
		return
	}
	expectedUpstreamID := playSession.ResumeScrobbleUpstreamID
	expectedSentAt := playSession.ResumeScrobbleSentAt
	_ = h.playbackStore.Update(playSession.ID, func(current *PlaybackSession) error {
		if current.ResumeScrobbleUpstreamID == expectedUpstreamID && current.ResumeScrobbleSentAt.Equal(expectedSentAt) {
			current.ResumeScrobbleUpstreamID = ""
		}
		return nil
	})
	playSession.ResumeScrobbleUpstreamID = ""
}

// compatResumeScrobbleNeedsCorrection reports whether a playing report shows
// the start scrobble placed playback in the wrong spot. While playing, the
// provider advances from the start's position, so the report is compared with
// that extrapolation. A zero report is never a resume point: clients send one
// while still seeking, and acting on it would replace a correct StartTimeTicks
// start with zero.
func compatResumeScrobbleNeedsCorrection(playSession *PlaybackSession, reportedSeconds float64, now time.Time) bool {
	if playSession == nil || reportedSeconds <= 0 || playSession.ResumeScrobbleUpstreamID == "" ||
		playSession.ResumeScrobbleUpstreamID != playSession.UpstreamSessionID {
		return false
	}
	elapsed := now.Sub(playSession.ResumeScrobbleSentAt)
	if elapsed > compatResumeScrobbleWindow {
		return false
	}
	expected := playSession.ResumeScrobblePosition + math.Max(elapsed.Seconds(), 0)
	return math.Abs(reportedSeconds-expected) > compatResumeScrobbleTolerance
}

func (h *PlaybackHandler) dispatchCompatScrobbleEvent(
	ctx context.Context,
	action compatScrobbleAction,
	event watchsync.ScrobbleEvent,
) error {
	return h.dispatchCompatScrobbleEventConfirmed(ctx, action, event, false)
}

func (h *PlaybackHandler) dispatchCompatScrobbleEventConfirmed(
	ctx context.Context,
	action compatScrobbleAction,
	event watchsync.ScrobbleEvent,
	confirmStop bool,
) error {
	if h == nil || h.WatchScrobbler == nil {
		return nil
	}
	scrobbleCtx, cancel := compatDetachedContext(ctx)
	defer cancel()
	var err error
	switch action {
	case compatScrobblePause:
		err = h.WatchScrobbler.ScrobblePause(scrobbleCtx, event)
	case compatScrobbleStop:
		if confirmer, ok := h.WatchScrobbler.(PlaybackWatchStopConfirmer); confirmStop && ok {
			err = confirmer.ScrobbleStopConfirmed(scrobbleCtx, event)
		} else if confirmStop {
			err = errors.New("watch scrobbler does not support confirmed stops")
		} else {
			err = h.WatchScrobbler.ScrobbleStop(scrobbleCtx, event)
		}
	default:
		err = h.WatchScrobbler.ScrobbleStart(scrobbleCtx, event)
	}
	if err != nil {
		slog.WarnContext(scrobbleCtx, "failed to queue jellycompat watch provider scrobble",
			"component", "jellycompat",
			"action", action,
			"playback_session_id", event.PlaybackSessionID,
			"error", err,
		)
	}
	return err
}

func compatScrobbleSource(
	playSession *PlaybackSession,
	upstreamSession *playback.Session,
	preferredSource *PlaybackMediaSource,
) *PlaybackMediaSource {
	if preferredSource != nil {
		return preferredSource
	}
	if playSession == nil {
		return nil
	}
	if upstreamSession != nil {
		for _, source := range playSession.MediaSources {
			if source.FileID == upstreamSession.MediaFileID {
				copy := source
				return &copy
			}
		}
	}
	return firstMediaSource(playSession)
}

// compatScrobbleFallbackSession preserves enough authenticated report state to
// emit a terminal event after the in-memory upstream session has already been
// reaped. The compat play session remains the source of media identity.
func compatScrobbleFallbackSession(
	compatSession *Session,
	playSession *PlaybackSession,
	preferredSource *PlaybackMediaSource,
	position float64,
	positionKnown bool,
	isPaused bool,
) *playback.Session {
	if compatSession == nil || playSession == nil || playSession.UpstreamSessionID == "" || !positionKnown {
		return nil
	}
	source := compatScrobbleSource(playSession, nil, preferredSource)
	fileID := 0
	if source != nil {
		fileID = source.FileID
	}
	return &playback.Session{
		ID:                         playSession.UpstreamSessionID,
		UserID:                     compatSession.StreamAppUserID,
		ProfileID:                  compatSession.ProfileID,
		MediaFileID:                fileID,
		Position:                   position,
		IsPaused:                   isPaused,
		DisableProgressPersistence: !playSession.ProgressPersistenceKnown || playSession.DisableProgressPersistence,
	}
}
