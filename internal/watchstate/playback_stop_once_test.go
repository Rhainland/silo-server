package watchstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// failingProgressStore fails progress writes, as a dropped database
// connection between the history insert and the progress write would.
type failingProgressStore struct{ userstore.UserStore }

func (s failingProgressStore) AddVisibleHistory(ctx context.Context, entry userstore.WatchHistoryEntry) (userstore.WatchHistoryEntry, error) {
	return userstore.AddVisibleHistory(ctx, s.UserStore, entry)
}

func (failingProgressStore) SetProgress(context.Context, string, string, float64, float64, userstore.ProgressThresholds) error {
	return errors.New("connection reset")
}

// TestRecordPlaybackStopOnceRecordsAPlayOnce covers a play several stops
// report: the first stop records it, a repeat changes nothing, and a stop
// whose progress write fails after its row is stored still reports and
// notifies the completed play, because no later stop will revisit it.
func TestRecordPlaybackStopOnceRecordsAPlayOnce(t *testing.T) {
	const historyID = "4b1e9d2c-7a35-5f60-8c14-2d9e7f0b3a61"
	stop := func(service *Service, position float64) (PlaybackStopResult, error) {
		return service.RecordPlaybackStopOnce(t.Context(), 1, "profile-1", "movie-1", 3600, position,
			time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC), userstore.VersionHints{}, userstore.ProgressThresholds{}, historyID)
	}

	t.Run("repeat", func(t *testing.T) {
		store, db := newTestUserStore(t)
		defer func() { _ = db.Close() }()
		createWatchstateProfile(t, store)
		observer := &completionRecorder{}
		service := NewService(testStoreProvider{store: store}).WithCompletionObserver(observer)

		if result, err := stop(service, 3500); err != nil || !result.Completed || result.AlreadyRecorded {
			t.Fatalf("first stop = %+v, %v; want a completed, newly recorded play", result, err)
		}
		if result, err := stop(service, 1800); err != nil || !result.AlreadyRecorded {
			t.Fatalf("repeated stop = %+v, %v; want AlreadyRecorded", result, err)
		}
		history, err := store.ListHistory(t.Context(), "profile-1", 10, 0)
		if err != nil || len(history) != 1 || history[0].ID != historyID {
			t.Fatalf("history = %+v, %v; want the one row %s", history, err, historyID)
		}
		progress, err := store.GetProgress(t.Context(), "profile-1", "movie-1")
		if err != nil || progress == nil || !progress.Completed {
			t.Fatalf("progress = %+v, %v; want still completed after the repeated stop", progress, err)
		}
		if len(observer.ids) != 1 {
			t.Fatalf("completion notifications = %v, want one", observer.ids)
		}
	})

	t.Run("progress write fails", func(t *testing.T) {
		store, db := newTestUserStore(t)
		defer func() { _ = db.Close() }()
		createWatchstateProfile(t, store)
		observer := &completionRecorder{}
		service := NewService(testStoreProvider{store: failingProgressStore{store}}).WithCompletionObserver(observer)

		result, err := stop(service, 3500)
		if err == nil {
			t.Fatal("stop with a failed progress write returned no error")
		}
		if !result.Completed || result.HistoryID != historyID {
			t.Fatalf("result = %+v, want the completed play %s", result, historyID)
		}
		if len(observer.ids) != 1 || observer.ids[0] != "movie-1" {
			t.Fatalf("completion notifications = %v, want movie-1", observer.ids)
		}
	})
}
