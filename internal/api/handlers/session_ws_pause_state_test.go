package handlers

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/playback"
)

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
