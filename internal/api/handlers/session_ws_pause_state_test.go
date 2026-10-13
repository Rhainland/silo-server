package handlers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/watchsync"
)

// waitConfirmedPauseStates waits until the session's queued confirmed pause
// and resume results have been applied.
func waitConfirmedPauseStates(t *testing.T, h *PlaybackHandler, sessionID string) {
	t.Helper()
	h.realtimeResultMu.Lock()
	queue := h.realtimeResultQueues[sessionID]
	h.realtimeResultMu.Unlock()
	if queue == nil {
		return
	}
	select {
	case <-queue.done:
	case <-time.After(5 * time.Second):
		t.Fatal("confirmed pause state was not applied")
	}
}

// A pause or resume the player confirms is the session's state at once, so
// the admin views do not wait for the next progress report; a rejected
// command changes nothing.
func TestRealtimeConfirmedPauseAndResumeSetSessionState(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(sessionMgr)
	handler.CommandTracker = playback.NewCommandTracker()
	defer handler.CommandTracker.Close()

	session, err := sessionMgr.StartSession(1, "profile-1", 100, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	paused := func() bool {
		t.Helper()
		current, err := sessionMgr.GetSession(session.ID)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		return current.IsPaused
	}
	send := func(commandID string, name playback.CommandName, status playback.RealtimeResultStatus) {
		t.Helper()
		handler.rememberRealtimeCommand(commandID, session.ID, name)
		if err := handler.handleRealtimeClientMessage(session.ID, realtimeResultMessage(t, session.ID, commandID, status)); err != nil {
			t.Fatalf("handleRealtimeClientMessage: %v", err)
		}
		waitConfirmedPauseStates(t, handler, session.ID)
	}

	send("cmd-pause-rejected", playback.CommandPause, playback.RealtimeResultStatusRejected)
	if paused() {
		t.Fatal("a rejected pause paused the session")
	}
	send("cmd-pause", playback.CommandPause, playback.RealtimeResultStatusCompleted)
	if !paused() {
		t.Fatal("a confirmed pause left the session playing")
	}
	send("cmd-resume", playback.CommandUnpause, playback.RealtimeResultStatusCompleted)
	if paused() {
		t.Fatal("a confirmed resume left the session paused")
	}
}

// A confirmed pause or resume sends the watch-provider transition itself; a
// progress report that follows sees no change and must not send it again.
func TestRealtimeConfirmedPauseAndResumeScrobbleOnce(t *testing.T) {
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", Duration: 3600}
	sessionMgr := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.CommandTracker = playback.NewCommandTracker()
	defer handler.CommandTracker.Close()
	scrobbler := &recordingPlaybackWatchScrobbler{}
	handler.WatchScrobbler = scrobbler

	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	send := func(commandID string, name playback.CommandName) {
		t.Helper()
		handler.rememberRealtimeCommand(commandID, session.ID, name)
		if err := handler.handleRealtimeClientMessage(session.ID, realtimeResultMessage(t, session.ID, commandID, playback.RealtimeResultStatusCompleted)); err != nil {
			t.Fatalf("handleRealtimeClientMessage: %v", err)
		}
		waitConfirmedPauseStates(t, handler, session.ID)
	}

	send("cmd-pause", playback.CommandPause)
	handler.writeProgressSideEffectsV2(context.Background(), nil, session.ID, playback.ProgressSampleV3{Sequence: 1, Position: 30, IsPaused: true})
	if len(scrobbler.pauses) != 1 || len(scrobbler.starts) != 0 {
		t.Fatalf("after pause: pauses = %d, starts = %d, want 1, 0", len(scrobbler.pauses), len(scrobbler.starts))
	}
	send("cmd-resume", playback.CommandUnpause)
	handler.writeProgressSideEffectsV2(context.Background(), nil, session.ID, playback.ProgressSampleV3{Sequence: 2, Position: 31})
	if len(scrobbler.pauses) != 1 || len(scrobbler.starts) != 1 {
		t.Fatalf("after resume: pauses = %d, starts = %d, want 1, 1", len(scrobbler.pauses), len(scrobbler.starts))
	}
}

// blockingPauseScrobbler holds every pause scrobble until released, like a
// provider whose token refresh is slow.
type blockingPauseScrobbler struct {
	release chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	actions []string
}

func (s *blockingPauseScrobbler) record(action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = append(s.actions, action)
}

func (s *blockingPauseScrobbler) ScrobbleStart(context.Context, watchsync.ScrobbleEvent) error {
	s.record("start")
	return nil
}

func (s *blockingPauseScrobbler) ScrobblePause(context.Context, watchsync.ScrobbleEvent) error {
	if s.entered != nil {
		close(s.entered)
	}
	<-s.release
	s.record("pause")
	return nil
}

func (s *blockingPauseScrobbler) ScrobbleStop(context.Context, watchsync.ScrobbleEvent) error {
	s.record("stop")
	return nil
}

// A slow watch-provider scrobble must not hold up the socket reader, and the
// confirmed pause and resume still apply in the order the player sent them.
func TestRealtimeConfirmedPauseDoesNotBlockReader(t *testing.T) {
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", Duration: 3600}
	sessionMgr := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.CommandTracker = playback.NewCommandTracker()
	defer handler.CommandTracker.Close()
	scrobbler := &blockingPauseScrobbler{release: make(chan struct{})}
	handler.WatchScrobbler = scrobbler

	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	reader := make(chan error, 1)
	go func() {
		for _, cmd := range []struct {
			id   string
			name playback.CommandName
		}{{"cmd-pause", playback.CommandPause}, {"cmd-resume", playback.CommandUnpause}} {
			handler.rememberRealtimeCommand(cmd.id, session.ID, cmd.name)
			if err := handler.handleRealtimeClientMessage(session.ID, realtimeResultMessage(t, session.ID, cmd.id, playback.RealtimeResultStatusCompleted)); err != nil {
				reader <- err
				return
			}
		}
		reader <- nil
	}()
	select {
	case err := <-reader:
		if err != nil {
			t.Fatalf("handleRealtimeClientMessage: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the socket reader waited on the watch-provider scrobble")
	}

	close(scrobbler.release)
	waitConfirmedPauseStates(t, handler, session.ID)
	scrobbler.mu.Lock()
	actions := append([]string(nil), scrobbler.actions...)
	scrobbler.mu.Unlock()
	if len(actions) != 2 || actions[0] != "pause" || actions[1] != "start" {
		t.Fatalf("scrobbles = %v, want [pause start]", actions)
	}
	current, err := sessionMgr.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if current.IsPaused {
		t.Fatal("session paused after a confirmed pause then resume")
	}
}

// A stop ends session authority even while a provider call is blocked.
// Pending resume state is discarded; provider finalization still runs last.
func TestRealtimeConfirmedStopRevokesAuthorityBeforeProviderFinalization(t *testing.T) {
	file := &models.MediaFile{ID: 42, ContentID: "movie-1", Duration: 3600}
	sessionMgr := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(sessionMgr, testPlaybackFileResolver{file: file})
	handler.CommandTracker = playback.NewCommandTracker()
	defer handler.CommandTracker.Close()
	scrobbler := &blockingPauseScrobbler{release: make(chan struct{}), entered: make(chan struct{})}
	handler.WatchScrobbler = scrobbler

	session, err := sessionMgr.StartSession(1, "profile-1", file.ID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	release := sync.OnceFunc(func() { close(scrobbler.release) })
	defer func() { release(); waitConfirmedPauseStates(t, handler, session.ID) }()
	handler.rememberRealtimeCommand("cmd-pause", session.ID, playback.CommandPause)
	if err := handler.handleRealtimeClientMessage(session.ID, realtimeResultMessage(t, session.ID, "cmd-pause", playback.RealtimeResultStatusCompleted)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-scrobbler.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("pause scrobble did not start")
	}
	reader := make(chan error, 1)
	go func() {
		for _, cmd := range []struct {
			id   string
			name playback.CommandName
		}{{"cmd-resume", playback.CommandUnpause}, {"cmd-stop", playback.CommandStop}} {
			handler.rememberRealtimeCommand(cmd.id, session.ID, cmd.name)
			if err := handler.handleRealtimeClientMessage(session.ID, realtimeResultMessage(t, session.ID, cmd.id, playback.RealtimeResultStatusCompleted)); err != nil {
				reader <- err
				return
			}
		}
		reader <- nil
	}()
	select {
	case err := <-reader:
		if err != nil {
			t.Fatalf("handleRealtimeClientMessage: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the socket reader waited on the watch-provider scrobble")
	}
	if _, err := sessionMgr.GetSession(session.ID); !errors.Is(err, playback.ErrSessionNotFound) {
		t.Fatalf("confirmed stop left the session live while provider was blocked: %v", err)
	}

	scrobbler.mu.Lock()
	if len(scrobbler.actions) != 0 {
		t.Errorf("provider finalization ran before pause release: %v", scrobbler.actions)
	}
	scrobbler.mu.Unlock()
	release()
	waitConfirmedPauseStates(t, handler, session.ID)
	scrobbler.mu.Lock()
	actions := append([]string(nil), scrobbler.actions...)
	scrobbler.mu.Unlock()
	if len(actions) != 2 || actions[0] != "pause" || actions[1] != "stop" {
		t.Fatalf("scrobbles = %v, want [pause stop]", actions)
	}
	if _, err := sessionMgr.GetSession(session.ID); err == nil {
		t.Fatal("session still live after a confirmed stop")
	}
}

func TestRealtimeConfirmationSurvivesAcceptedProgressReplay(t *testing.T) {
	for _, paused := range []bool{true, false} {
		name := "pause"
		commandName := playback.CommandPause
		if !paused {
			name = "resume"
			commandName = playback.CommandUnpause
		}
		t.Run(name, func(t *testing.T) {
			f := newPlaybackServiceFixture(t)
			f.handler.fileResolver = testPlaybackFileResolver{file: &models.MediaFile{ID: 100, ContentID: "movie-1", Duration: 3600}}
			scrobbler := &recordingPlaybackWatchScrobbler{}
			f.handler.WatchScrobbler = scrobbler
			sample := PlaybackProgressCommand{Sequence: 1, Position: 30, IsPaused: !paused}
			if _, err := f.handler.ApplyProgressV2(f.ctx, f.caller, f.session.ID, sample); err != nil {
				t.Fatal(err)
			}
			f.handler.rememberRealtimeCommand("cmd-confirm", f.session.ID, commandName)
			if err := f.handler.handleRealtimeClientMessage(f.session.ID, realtimeResultMessage(t, f.session.ID, "cmd-confirm", playback.RealtimeResultStatusCompleted)); err != nil {
				t.Fatal(err)
			}
			waitConfirmedPauseStates(t, f.handler, f.session.ID)
			pauseCount, startCount := len(scrobbler.pauses), len(scrobbler.starts)
			receipt, err := f.handler.ApplyProgressV2(f.ctx, f.caller, f.session.ID, sample)
			if err != nil || receipt.Outcome != PlaybackOutcomeReplayed {
				t.Fatalf("replay = %+v, %v", receipt, err)
			}
			current, err := f.manager.GetSession(f.session.ID)
			if err != nil || current.IsPaused != paused || current.Position != 30 {
				t.Fatalf("replayed state = %+v, %v", current, err)
			}
			if len(scrobbler.pauses) != pauseCount || len(scrobbler.starts) != startCount {
				t.Fatal("progress replay emitted another provider transition")
			}
			sample.Sequence = 2
			sample.Position = 31
			if _, err := f.handler.ApplyProgressV2(f.ctx, f.caller, f.session.ID, sample); err != nil {
				t.Fatal(err)
			}
			current, err = f.manager.GetSession(f.session.ID)
			if err != nil || current.IsPaused == paused || current.Position != 31 {
				t.Fatalf("newer progress = %+v, %v", current, err)
			}
		})
	}
}

func TestRealtimeResultQueueBoundsPendingPauseStates(t *testing.T) {
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	entered := make(chan struct{})
	release := make(chan struct{})
	released := sync.OnceFunc(func() { close(release) })
	defer func() { released(); waitConfirmedPauseStates(t, h, "session-queue") }()
	h.runRealtimeResult("session-queue", true, func() { close(entered); <-release })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	var mu sync.Mutex
	var applied []int
	for i := range 1000 {
		h.runRealtimeResult("session-queue", true, func() { mu.Lock(); applied = append(applied, i); mu.Unlock() })
	}
	h.realtimeResultMu.Lock()
	count := len(h.realtimeResultQueues["session-queue"].pending)
	h.realtimeResultMu.Unlock()
	if count != maxPendingRealtimeResults {
		t.Fatalf("pending results = %d", count)
	}
	terminal := make(chan struct{})
	h.runRealtimeResult("session-queue", false, func() { close(terminal) })
	// Results received after terminal authority was removed cannot replace
	// the finalization, even when the pending queue is already full.
	for range 100 {
		h.runRealtimeResult("session-queue", true, func() { t.Error("late pause applied after terminal result") })
	}
	released()
	waitConfirmedPauseStates(t, h, "session-queue")
	select {
	case <-terminal:
	default:
		t.Fatal("terminal finalization was dropped")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != maxPendingRealtimeResults || applied[len(applied)-1] != 999 {
		t.Fatalf("applied states = %v", applied)
	}
	for i := 1; i < len(applied); i++ {
		if applied[i] <= applied[i-1] {
			t.Fatalf("results out of order: %v", applied)
		}
	}
	h.realtimeResultMu.Lock()
	_, retained := h.realtimeResultQueues["session-queue"]
	h.realtimeResultMu.Unlock()
	if retained {
		t.Fatal("drained queue retained")
	}
}

func TestRealtimeDelayedConfirmationPreservesNewerProgress(t *testing.T) {
	f := newPlaybackServiceFixture(t)
	if _, err := f.handler.ApplyProgressV2(f.ctx, f.caller, f.session.ID, PlaybackProgressCommand{Sequence: 1, Position: 30}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	released := sync.OnceFunc(func() { close(release) })
	defer func() { released(); waitConfirmedPauseStates(t, f.handler, f.session.ID) }()
	f.handler.runRealtimeResult(f.session.ID, true, func() { close(entered); <-release })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	f.handler.rememberRealtimeCommand("cmd-delayed-pause", f.session.ID, playback.CommandPause)
	if err := f.handler.handleRealtimeClientMessage(f.session.ID, realtimeResultMessage(t, f.session.ID, "cmd-delayed-pause", playback.RealtimeResultStatusCompleted)); err != nil {
		t.Fatal(err)
	}
	// The player resumed locally after its pause confirmation. This newer
	// observation reaches the server while the old confirmation is queued.
	if _, err := f.handler.ApplyProgressV2(f.ctx, f.caller, f.session.ID, PlaybackProgressCommand{Sequence: 2, Position: 31}); err != nil {
		t.Fatal(err)
	}
	released()
	waitConfirmedPauseStates(t, f.handler, f.session.ID)
	current, err := f.manager.GetSession(f.session.ID)
	if err != nil || current.IsPaused || current.Position != 31 {
		t.Fatalf("newer progress overwritten by queued confirmation: %+v, %v", current, err)
	}
}

func TestRealtimeDelayedConfirmationUsesNewerReplicaProgress(t *testing.T) {
	f := newPlaybackServiceFixture(t)
	if _, err := f.handler.ApplyProgressV2(f.ctx, f.caller, f.session.ID, PlaybackProgressCommand{Sequence: 1, Position: 30, IsPaused: true}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	released := sync.OnceFunc(func() { close(release) })
	defer func() { released(); waitConfirmedPauseStates(t, f.handler, f.session.ID) }()
	f.handler.runRealtimeResult(f.session.ID, true, func() { close(entered); <-release })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	f.handler.rememberRealtimeCommand("cmd-delayed-resume", f.session.ID, playback.CommandUnpause)
	if err := f.handler.handleRealtimeClientMessage(f.session.ID, realtimeResultMessage(t, f.session.ID, "cmd-delayed-resume", playback.RealtimeResultStatusCompleted)); err != nil {
		t.Fatal(err)
	}
	// Another replica accepts playing progress but does not hold this live
	// session. The result worker must use the durable observation locally.
	store := f.handler.PlanStoreV3.(playback.ProgressStoreV3)
	if _, err := store.ApplyProgress(f.ctx, f.session.ID, playback.ProgressSampleV3{Sequence: 2, Position: 31}); err != nil {
		t.Fatal(err)
	}
	released()
	waitConfirmedPauseStates(t, f.handler, f.session.ID)
	current, err := f.manager.GetSession(f.session.ID)
	if err != nil || current.IsPaused {
		t.Fatalf("newer replica progress not reflected locally: %+v, %v", current, err)
	}
}
