package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
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
