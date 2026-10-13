package handlers

import (
	"context"
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
	tail := h.realtimeResultTails[sessionID]
	h.realtimeResultMu.Unlock()
	if tail == nil {
		return
	}
	select {
	case <-tail:
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

// A stop the player confirms while an earlier resume is still being applied
// runs after it, so the provider never sees a start after the stop, and the
// socket reader still does not wait on either.
func TestRealtimeConfirmedStopWaitsForQueuedResume(t *testing.T) {
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
		}{{"cmd-pause", playback.CommandPause}, {"cmd-resume", playback.CommandUnpause}, {"cmd-stop", playback.CommandStop}} {
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
	if _, err := sessionMgr.GetSession(session.ID); err != nil {
		t.Fatalf("the stop ran before the queued pause and resume: %v", err)
	}

	close(scrobbler.release)
	waitConfirmedPauseStates(t, handler, session.ID)
	scrobbler.mu.Lock()
	actions := append([]string(nil), scrobbler.actions...)
	scrobbler.mu.Unlock()
	if len(actions) != 3 || actions[0] != "pause" || actions[1] != "start" || actions[2] != "stop" {
		t.Fatalf("scrobbles = %v, want [pause start stop]", actions)
	}
	if _, err := sessionMgr.GetSession(session.ID); err == nil {
		t.Fatal("session still live after a confirmed stop")
	}
}
