package playback

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func markerSnapshotFile(fileID int) *models.MediaFile {
	introStart, introEnd := 12.0, 75.0
	return &models.MediaFile{ID: fileID, IntroStart: &introStart, IntroEnd: &introEnd}
}

func TestMarkerSnapshotTargetsOnlyItsRegistration(t *testing.T) {
	sessions := NewSessionManager(0, 0)
	session, _ := sessions.StartSession(1, "profile-a", 100, PlayDirect, false)
	hub := NewRealtimeHub()
	first := &dispatchTestConn{}
	staleReg := hub.Register(session.ID, first)
	second := &dispatchTestConn{}
	currentReg := hub.Register(session.ID, second)
	defer hub.Unregister(currentReg)
	notifier := NewMarkerUpdateNotifier(sessions, hub)
	load := func(context.Context, int) (*models.MediaFile, error) { return markerSnapshotFile(100), nil }

	// A snapshot started for a connection that has since been replaced must
	// not reach the replacement.
	if sent, err := notifier.SendSnapshot(context.Background(), staleReg, 100, load); sent || err != nil {
		t.Fatalf("stale registration: sent=%v err=%v", sent, err)
	}
	if len(second.sent()) != 0 || len(first.sent()) != 0 {
		t.Fatalf("stale snapshot delivered: first=%d second=%d", len(first.sent()), len(second.sent()))
	}

	sent, err := notifier.SendSnapshot(context.Background(), currentReg, 100, load)
	if !sent || err != nil {
		t.Fatalf("current registration: sent=%v err=%v", sent, err)
	}
	messages := second.sent()
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	event := messages[0].(EventEnvelope)
	var payload MarkersUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if event.Name != RealtimeEventMarkersUpdated || payload.SessionID != session.ID || payload.FileID != 100 || payload.Intro == nil || payload.Intro.Start != 12 {
		t.Fatalf("event = %#v payload = %#v", event, payload)
	}
}

func TestMarkerSnapshotSkipsFilesWithoutMarkers(t *testing.T) {
	sessions := NewSessionManager(0, 0)
	session, _ := sessions.StartSession(1, "profile-a", 100, PlayDirect, false)
	hub := NewRealtimeHub()
	conn := &dispatchTestConn{}
	reg := hub.Register(session.ID, conn)
	defer hub.Unregister(reg)
	notifier := NewMarkerUpdateNotifier(sessions, hub)

	// An all-empty event would clear whatever the player already shows.
	sent, err := notifier.SendSnapshot(context.Background(), reg, 100, func(context.Context, int) (*models.MediaFile, error) {
		return &models.MediaFile{ID: 100}, nil
	})
	if sent || err != nil || len(conn.sent()) != 0 {
		t.Fatalf("empty row: sent=%v err=%v messages=%d", sent, err, len(conn.sent()))
	}

	loadErr := errors.New("database unavailable")
	if _, err := notifier.SendSnapshot(context.Background(), reg, 100, func(context.Context, int) (*models.MediaFile, error) {
		return nil, loadErr
	}); !errors.Is(err, loadErr) {
		t.Fatalf("load error = %v, want %v", err, loadErr)
	}
}

func TestMarkerSnapshotYieldsToAnUpdateDeliveredDuringTheRead(t *testing.T) {
	sessions := NewSessionManager(0, 0)
	session, _ := sessions.StartSession(1, "profile-a", 100, PlayDirect, false)
	_ = sessions.SetRealtimeConnection(session.ID, true)
	hub := NewRealtimeHub()
	conn := &dispatchTestConn{}
	reg := hub.Register(session.ID, conn)
	defer hub.Unregister(reg)
	notifier := NewMarkerUpdateNotifier(sessions, hub)

	creditsStart, creditsEnd := 3600.0, 3660.0
	newer := &models.MediaFile{ID: 100, CreditsStart: &creditsStart, CreditsEnd: &creditsEnd}
	loads := 0
	sent, err := notifier.SendSnapshot(context.Background(), reg, 100, func(ctx context.Context, _ int) (*models.MediaFile, error) {
		loads++
		if loads == 1 {
			// A provider result is stored and delivered after this read saw
			// the old row; the re-read sees the stored result.
			notifier.MarkersUpdated(ctx, newer)
			return markerSnapshotFile(100), nil
		}
		return newer, nil
	})
	if !sent || err != nil || loads != 2 {
		t.Fatalf("snapshot after a newer update: sent=%v err=%v loads=%d", sent, err, loads)
	}
	// The stale intro-only row must never reach the player: it would clear the
	// newer credits.
	for i, message := range conn.sent() {
		var payload MarkersUpdatedPayload
		if err := json.Unmarshal(message.(EventEnvelope).Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Credits == nil || payload.Intro != nil {
			t.Fatalf("message %d = %#v, want only the newer credits-only state", i, payload)
		}
	}
}

func TestMarkerSnapshotSurvivesAnUpdateToAnotherFileInTheSameStripe(t *testing.T) {
	sessions := NewSessionManager(0, 0)
	session, _ := sessions.StartSession(1, "profile-a", 100, PlayDirect, false)
	hub := NewRealtimeHub()
	conn := &dispatchTestConn{}
	reg := hub.Register(session.ID, conn)
	defer hub.Unregister(reg)
	notifier := NewMarkerUpdateNotifier(sessions, hub)

	sameStripe := 100 + markerEpochStripes
	loads := 0
	sent, err := notifier.SendSnapshot(context.Background(), reg, 100, func(ctx context.Context, _ int) (*models.MediaFile, error) {
		loads++
		if loads == 1 {
			notifier.MarkersUpdated(ctx, markerSnapshotFile(sameStripe))
		}
		return markerSnapshotFile(100), nil
	})
	if !sent || err != nil {
		t.Fatalf("an unrelated file's update cancelled the snapshot: sent=%v err=%v loads=%d", sent, err, loads)
	}
	if len(conn.sent()) != 1 {
		t.Fatalf("messages = %d, want the snapshot", len(conn.sent()))
	}
}
