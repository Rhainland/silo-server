package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/watchstate"
)

type playbackCommandRecord struct {
	SessionID string
	Name      playback.CommandName
}

// stopPlaybackSession stops a session. userInitiated must be true only for a
// genuine user/admin stop (the recipe card is then deleted); false for system
// teardown such as an ffmpeg-exit cleanup, where the card is kept so the client
// can reconstruct.
func (h *PlaybackHandler) stopPlaybackSession(ctx context.Context, session *playback.Session, userInitiated bool) error {
	_, err := h.stopPlaybackSessionWithResult(ctx, session, userInitiated)
	return err
}

// stopPlaybackSessionWithResult is stopPlaybackSession reporting the history
// writer's result, which the v2 stop receipt carries.
func (h *PlaybackHandler) stopPlaybackSessionWithResult(ctx context.Context, session *playback.Session, userInitiated bool) (watchstate.PlaybackStopResult, error) {
	if h == nil || session == nil || session.ID == "" {
		return watchstate.PlaybackStopResult{}, playback.ErrSessionNotFound
	}
	unlock := h.tm.LockSessionLifecycle(session.ID)
	defer unlock()
	current, err := h.endPlaybackSessionAuthority(ctx, session, userInitiated)
	if err != nil {
		return watchstate.PlaybackStopResult{}, err
	}
	return h.finalizeSessionStopWithResult(ctx, current, true, "stop", userInitiated), nil
}

// endPlaybackSessionAuthority ends the live session and revokes its streams
// before optional provider notifications run. The returned snapshot is used
// for history and ordered provider finalization. The caller holds the
// session lifecycle lock through authority and transport cleanup.
func (h *PlaybackHandler) endPlaybackSessionAuthority(ctx context.Context, session *playback.Session, userInitiated bool) (*playback.Session, error) {
	if h == nil || session == nil || session.ID == "" {
		return nil, playback.ErrSessionNotFound
	}
	// Replacement preparation holds this lifecycle lock until its new live
	// session state and durable plan are committed (or rolled back). Wait for
	// that boundary, then reload the route: callers commonly hold a copy taken
	// before the replacement started, and deleting authority from that stale
	// copy would leave the successor transport playable after a successful stop.
	current, err := h.sessionMgr.GetSession(session.ID)
	if err != nil {
		return nil, err
	}
	session = current
	if userInitiated {
		if err := h.deleteRequiredProgressiveRemuxAuthorityV3(ctx, session); err != nil {
			return nil, fmt.Errorf("persist progressive remux stop: %w", err)
		}
		if requiresProgressiveRemuxAuthorityV3(session) {
			if err := h.tm.CancelRemoteTranscode(ctx, remoteTransportID(session), session.TranscodeNodeURL); err != nil {
				return nil, fmt.Errorf("cancel progressive remux transport: %w", err)
			}
		}
	}

	if err := h.sessionMgr.StopSession(session.ID); err != nil {
		return nil, err
	}
	if userInitiated {
		// A user or admin stop is final: revoke the session's stream tokens on
		// every replica, not just the live entry here.
		h.StreamDeny.Deny(ctx, session.ID)
	}
	return session, nil
}

func (h *PlaybackHandler) stopPlaybackSessionByID(ctx context.Context, sessionID string, userInitiated bool) error {
	if h == nil || sessionID == "" {
		return playback.ErrSessionNotFound
	}
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil {
		return err
	}
	return h.stopPlaybackSession(ctx, session, userInitiated)
}

func (h *PlaybackHandler) abortPlaybackSession(ctx context.Context, session *playback.Session) error {
	if h == nil || session == nil || session.ID == "" {
		return playback.ErrSessionNotFound
	}

	if err := h.sessionMgr.StopSession(session.ID); err != nil {
		return err
	}
	h.finalizeSessionAbort(ctx, session, true, "abort")
	h.markAttemptStoppedServerSide(ctx, session.ID)
	return nil
}

func (h *PlaybackHandler) abortPlaybackSessionByID(ctx context.Context, sessionID string) error {
	if h == nil || sessionID == "" {
		return playback.ErrSessionNotFound
	}
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil {
		return err
	}
	return h.abortPlaybackSession(ctx, session)
}

// CopySafetyPlaybackControl adapts the playback handler to the session control
// playback.CopySafetyNotifier needs: the notifier owns the decision to withdraw
// a plan, the handler owns realtime command bookkeeping and session teardown.
type CopySafetyPlaybackControl struct {
	playback *PlaybackHandler
}

// NewCopySafetyPlaybackControl returns the adapter, or nil without a handler.
func NewCopySafetyPlaybackControl(handler *PlaybackHandler) *CopySafetyPlaybackControl {
	if handler == nil {
		return nil
	}
	return &CopySafetyPlaybackControl{playback: handler}
}

func (c *CopySafetyPlaybackControl) RememberRealtimeCommand(commandID, sessionID string, name playback.CommandName) {
	if c == nil {
		return
	}
	c.playback.rememberRealtimeCommand(commandID, sessionID, name)
}

func (c *CopySafetyPlaybackControl) ForgetRealtimeCommand(commandID string) {
	if c == nil {
		return
	}
	c.playback.forgetRealtimeCommand(commandID)
}

// StopSession ends the session as a system teardown, not a user stop: the
// recipe card is kept so the client's recovery can rebuild from it.
func (c *CopySafetyPlaybackControl) StopSession(ctx context.Context, sessionID string) error {
	if c == nil {
		return playback.ErrSessionNotFound
	}
	return c.playback.stopPlaybackSessionByID(ctx, sessionID, false)
}

func (h *PlaybackHandler) rememberRealtimeCommand(commandID, sessionID string, name playback.CommandName) {
	if h == nil || commandID == "" || sessionID == "" {
		return
	}

	h.realtimeCommandMu.Lock()
	h.realtimeCommands[commandID] = playbackCommandRecord{
		SessionID: sessionID,
		Name:      name,
	}
	h.realtimeCommandMu.Unlock()
}

func (h *PlaybackHandler) forgetRealtimeCommand(commandID string) {
	if h == nil || commandID == "" {
		return
	}

	h.realtimeCommandMu.Lock()
	delete(h.realtimeCommands, commandID)
	h.realtimeCommandMu.Unlock()
}

func (h *PlaybackHandler) getRealtimeCommand(commandID string) (playbackCommandRecord, bool) {
	if h == nil || commandID == "" {
		return playbackCommandRecord{}, false
	}

	h.realtimeCommandMu.Lock()
	record, ok := h.realtimeCommands[commandID]
	h.realtimeCommandMu.Unlock()
	return record, ok
}

func (h *PlaybackHandler) setRealtimeConnectionState(sessionID string, connected bool) bool {
	if h == nil || sessionID == "" {
		return false
	}

	type realtimeStateSetter interface {
		SetRealtimeConnection(sessionID string, connected bool) error
	}

	mgr, ok := h.sessionMgr.(realtimeStateSetter)
	if !ok {
		return false
	}

	session, err := h.sessionMgr.GetSession(sessionID)
	if err == nil && session != nil && session.HasRealtimeConnection == connected {
		return false
	}

	if err := mgr.SetRealtimeConnection(sessionID, connected); err != nil {
		if errors.Is(err, playback.ErrSessionNotFound) {
			return false
		}
		slog.Warn("failed to update realtime connection state", "session", sessionID, "connected", connected, "error", err)
		return false
	}
	if h.StreamTelemetry != nil {
		h.StreamTelemetry.SetRealtimeConnection(sessionID, connected)
	}
	return true
}

const maxPendingRealtimeResults = 32

type realtimeResultQueue struct {
	pending        []func()
	done           chan struct{}
	terminalQueued bool
}

// runRealtimeResult orders results through one worker per session. When a
// slow provider fills the queue, its last pending pause state is superseded
// by the newest one. Terminal finalization is always retained: the caller
// has already removed session authority, so later terminal results cannot
// enqueue another finalization for the same session.
func (h *PlaybackHandler) runRealtimeResult(sessionID string, background bool, apply func()) {
	h.realtimeResultMu.Lock()
	if h.realtimeResultQueues == nil {
		h.realtimeResultQueues = make(map[string]*realtimeResultQueue)
	}
	if queue := h.realtimeResultQueues[sessionID]; queue != nil {
		if queue.terminalQueued {
			h.realtimeResultMu.Unlock()
			return
		}
		if !background {
			queue.terminalQueued = true
		}
		if background && len(queue.pending) >= maxPendingRealtimeResults {
			queue.pending[len(queue.pending)-1] = apply
		} else {
			queue.pending = append(queue.pending, apply)
		}
		h.realtimeResultMu.Unlock()
		return
	}
	queue := &realtimeResultQueue{done: make(chan struct{}), terminalQueued: !background}
	h.realtimeResultQueues[sessionID] = queue
	h.realtimeResultMu.Unlock()
	run := func() {
		apply()
		for {
			h.realtimeResultMu.Lock()
			if len(queue.pending) == 0 {
				delete(h.realtimeResultQueues, sessionID)
				close(queue.done)
				h.realtimeResultMu.Unlock()
				return
			}
			next := queue.pending[0]
			queue.pending[0] = nil
			queue.pending = queue.pending[1:]
			h.realtimeResultMu.Unlock()
			next()
		}
	}
	if background {
		go run()
		return
	}
	run()
}

// setConfirmedPauseState applies a pause or resume the player confirmed over
// its control lane. When the state changed it syncs the admin session views
// and sends the watch-provider pause or resume scrobble, which the next
// progress report would otherwise skip because it sees no transition.
func (h *PlaybackHandler) setConfirmedPauseState(sessionID string, paused bool, receivedSequence int64) {
	type pauseSetter interface {
		SetPaused(sessionID string, paused bool, acceptedSequence int64) (bool, error)
	}
	mgr, ok := h.sessionMgr.(pauseSetter)
	if !ok {
		return
	}
	// Progress side effects hold this lock while they compare the old and new
	// pause state, so a report racing this change scrobbles it at most once.
	unlock := h.progressSideEffectLock(sessionID)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	attempt, err := h.realtimeProgressAttempt(ctx, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "failed to read progress sequence for confirmed pause", "session", sessionID, "error", err)
		return
	}
	var acceptedSequence int64
	if attempt != nil {
		acceptedSequence = attempt.LastSequence
	}
	// Progress accepted after receipt is a newer player observation. Apply
	// its pause state: another replica may have accepted it without holding
	// this live session, so simply skipping would leave our local state stale.
	if acceptedSequence > receivedSequence {
		if attempt.LastSample == nil {
			return
		}
		paused = attempt.LastSample.IsPaused
	}
	changed, err := mgr.SetPaused(sessionID, paused, acceptedSequence)
	if err != nil {
		if !errors.Is(err, playback.ErrSessionNotFound) {
			slog.Warn("failed to apply confirmed pause state", "session", sessionID, "playback_session_id", sessionID, "error", err)
		}
		return
	}
	if !changed {
		return
	}
	h.syncSessionsNow(ctx, "realtime_pause")
	if current, err := h.sessionMgr.GetSession(sessionID); err == nil && current != nil {
		h.scrobblePauseTransitionV2(ctx, current, !paused)
	}
}

func (h *PlaybackHandler) realtimeProgressAttempt(ctx context.Context, sessionID string) (*playback.AttemptRecordV3, error) {
	if h.PlanStoreV3 == nil {
		return nil, nil
	}
	attempt, err := h.PlanStoreV3.GetAttempt(ctx, sessionID)
	if errors.Is(err, playback.ErrSessionNotFound) {
		return nil, nil // Bridge sessions may have no native attempt row.
	}
	if err != nil {
		return nil, err
	}
	return attempt, nil
}
