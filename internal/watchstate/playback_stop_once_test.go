package watchstate

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// TestRecordPlaybackStopOnceRecordsAPlayOnce covers a play several stops
// report: the first stop records it, a stop that completes it upgrades the
// same row, a further repeat changes nothing, and no stop writes progress.
func TestRecordPlaybackStopOnceRecordsAPlayOnce(t *testing.T) {
	const historyID = "4b1e9d2c-7a35-5f60-8c14-2d9e7f0b3a61"
	store, db := newTestUserStore(t)
	defer func() { _ = db.Close() }()
	createWatchstateProfile(t, store)
	observer := &completionRecorder{}
	service := NewService(testStoreProvider{store: store}).WithCompletionObserver(observer)
	stop := func(position float64) PlaybackStopResult {
		t.Helper()
		result, err := service.RecordPlaybackStopOnce(t.Context(), 1, "profile-1", "movie-1", 3600, position,
			time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC), userstore.VersionHints{}, userstore.ProgressThresholds{}, historyID)
		if err != nil {
			t.Fatalf("stop at %v: %v", position, err)
		}
		return result
	}

	if result := stop(1800); result.AlreadyRecorded || result.Completed {
		t.Fatalf("first stop = %+v, want a newly recorded, incomplete play", result)
	}
	if result := stop(3500); result.AlreadyRecorded || !result.Completed {
		t.Fatalf("completing stop = %+v, want the play completed", result)
	}
	if result := stop(3500); !result.AlreadyRecorded {
		t.Fatalf("repeated stop = %+v, want AlreadyRecorded", result)
	}

	history, err := store.ListHistory(t.Context(), "profile-1", 10, 0)
	if err != nil || len(history) != 1 || history[0].ID != historyID || !history[0].Completed {
		t.Fatalf("history = %+v, %v; want the one completed row %s", history, err, historyID)
	}
	if len(observer.ids) != 1 {
		t.Fatalf("completion notifications = %v, want one", observer.ids)
	}
	if progress, err := store.GetProgress(t.Context(), "profile-1", "movie-1"); err != nil || progress != nil {
		t.Fatalf("progress = %+v, %v; want none written by the stops", progress, err)
	}
}
