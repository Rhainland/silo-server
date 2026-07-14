package metadata

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/catalog"
)

type blockingArtworkRevisionDeleter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu      sync.Mutex
	deleted [][]string
}

func (d *blockingArtworkRevisionDeleter) Bucket() string { return "artwork" }

func (d *blockingArtworkRevisionDeleter) DeleteObjects(_ context.Context, _ string, keys []string) (int, error) {
	d.once.Do(func() { close(d.started) })
	if d.release != nil {
		<-d.release
	}
	d.mu.Lock()
	d.deleted = append(d.deleted, append([]string(nil), keys...))
	d.mu.Unlock()
	return len(keys), nil
}

func artworkRevisionGCTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestArtworkRevisionGCSerializesDeletionWithRetracking(t *testing.T) {
	pool := artworkRevisionGCTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	suffix := time.Now().UnixNano()
	originalPath := fmt.Sprintf("tmdb/movies/gc-%d/poster/original.old.webp", suffix)
	oldKeys := []string{originalPath, fmt.Sprintf("tmdb/movies/gc-%d/poster/w500.old.webp", suffix)}
	newKeys := []string{originalPath, fmt.Sprintf("tmdb/movies/gc-%d/poster/w500.old.webp", suffix), fmt.Sprintf("tmdb/movies/gc-%d/poster/w300.old.webp", suffix)}
	workerID := fmt.Sprintf("gc-worker-%d", suffix)

	var candidateID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO artwork_revision_gc_candidates (
			original_path, object_keys, not_before, next_attempt_at, locked_at, locked_by
		) VALUES ($1, $2, NOW() - interval '1 hour', NOW() - interval '1 hour', NOW(), $3)
		RETURNING id`, originalPath, oldKeys, workerID).Scan(&candidateID); err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM artwork_revision_gc_candidates WHERE original_path = $1`, originalPath)
	})

	deleter := &blockingArtworkRevisionDeleter{started: make(chan struct{}), release: make(chan struct{})}
	collector := NewArtworkRevisionGarbageCollector(pool, deleter)
	processDone := make(chan struct {
		outcome artworkRevisionGCOutcome
		err     error
	}, 1)
	go func() {
		outcome, err := collector.processCandidate(ctx, artworkRevisionGCCandidate{
			id: candidateID, originalPath: originalPath, objectKeys: oldKeys,
		}, workerID)
		processDone <- struct {
			outcome artworkRevisionGCOutcome
			err     error
		}{outcome: outcome, err: err}
	}()

	select {
	case <-deleter.started:
	case <-ctx.Done():
		t.Fatalf("collector did not begin deletion: %v", ctx.Err())
	}

	tracker := catalog.NewArtworkRevisionTracker(pool)
	trackDone := make(chan error, 1)
	go func() { trackDone <- tracker.TrackArtworkRevision(ctx, originalPath, newKeys) }()

	select {
	case err := <-trackDone:
		t.Fatalf("tracking completed while deletion row was locked: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(deleter.release)
	processed := <-processDone
	if processed.err != nil {
		t.Fatalf("processCandidate: %v", processed.err)
	}
	if processed.outcome != artworkRevisionGCDeleted {
		t.Fatalf("outcome = %v, want deleted", processed.outcome)
	}
	if err := <-trackDone; err != nil {
		t.Fatalf("retrack revision: %v", err)
	}

	var storedKeys []string
	var nextAttempt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT object_keys, next_attempt_at
		FROM artwork_revision_gc_candidates
		WHERE original_path = $1`, originalPath).Scan(&storedKeys, &nextAttempt); err != nil {
		t.Fatalf("load retracked revision: %v", err)
	}
	if !slices.Equal(storedKeys, newKeys) {
		t.Fatalf("stored manifest = %v, want %v", storedKeys, newKeys)
	}
	if !nextAttempt.After(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("next attempt = %v, want refreshed publication grace", nextAttempt)
	}
}

func TestArtworkRevisionGCParksReferencedRevision(t *testing.T) {
	pool := artworkRevisionGCTestPool(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("gc-referenced-%d", suffix)
	originalPath := fmt.Sprintf("tmdb/movies/%d/poster/original.live.webp", suffix)
	keys := []string{originalPath}
	workerID := fmt.Sprintf("gc-worker-%d", suffix)

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title, status, genres, poster_path)
		VALUES ($1, 'movie', 'GC Referenced', 'matched', '{}'::text[], $2)`, contentID, originalPath); err != nil {
		t.Fatalf("seed referenced item: %v", err)
	}
	var candidateID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO artwork_revision_gc_candidates (
			original_path, object_keys, not_before, next_attempt_at, locked_at, locked_by
		) VALUES ($1, $2, NOW() - interval '1 hour', NOW() - interval '1 hour', NOW(), $3)
		RETURNING id`, originalPath, keys, workerID).Scan(&candidateID); err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = $1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM artwork_revision_gc_candidates WHERE original_path = $1`, originalPath)
	})

	deleter := &blockingArtworkRevisionDeleter{started: make(chan struct{})}
	collector := NewArtworkRevisionGarbageCollector(pool, deleter)
	outcome, err := collector.processCandidate(ctx, artworkRevisionGCCandidate{id: candidateID}, workerID)
	if err != nil {
		t.Fatalf("processCandidate: %v", err)
	}
	if outcome != artworkRevisionGCReferenced {
		t.Fatalf("outcome = %v, want referenced", outcome)
	}
	select {
	case <-deleter.started:
		t.Fatal("referenced revision was sent to object deletion")
	default:
	}

	var lockedBy string
	var nextAttempt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT locked_by, next_attempt_at
		FROM artwork_revision_gc_candidates
		WHERE id = $1`, candidateID).Scan(&lockedBy, &nextAttempt); err != nil {
		t.Fatalf("load parked revision: %v", err)
	}
	if lockedBy != "" {
		t.Fatalf("locked_by = %q, want released", lockedBy)
	}
	if nextAttempt != nil {
		t.Fatalf("next_attempt_at = %v, want dormant NULL", nextAttempt)
	}
}

func TestArtworkRevisionTriggerQueuesDisplacedRevision(t *testing.T) {
	pool := artworkRevisionGCTestPool(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	contentID := fmt.Sprintf("gc-trigger-%d", suffix)
	oldPath := fmt.Sprintf("tmdb/movies/%d/poster/original.old.webp", suffix)
	newPath := fmt.Sprintf("tmdb/movies/%d/poster/original.new.webp", suffix)
	wantKeys := []string{
		oldPath,
		fmt.Sprintf("tmdb/movies/%d/poster/w500.old.webp", suffix),
		fmt.Sprintf("tmdb/movies/%d/poster/w300.old.webp", suffix),
		fmt.Sprintf("tmdb/movies/%d/poster/future-variant.old.webp", suffix),
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title, status, genres, poster_path)
		VALUES ($1, 'movie', 'GC Trigger', 'matched', '{}'::text[], $2)`, contentID, oldPath); err != nil {
		t.Fatalf("seed artwork: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artwork_revision_gc_candidates (
			original_path, object_keys, not_before, next_attempt_at
		) VALUES ($1, $2, NOW(), NULL)`, oldPath, wantKeys); err != nil {
		t.Fatalf("seed dormant manifest: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = $1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM artwork_revision_gc_candidates WHERE original_path IN ($1, $2)`, oldPath, newPath)
	})

	if _, err := pool.Exec(ctx, `UPDATE media_items SET poster_path = $2 WHERE content_id = $1`, contentID, newPath); err != nil {
		t.Fatalf("replace artwork: %v", err)
	}

	var objectKeys []string
	var notBefore time.Time
	if err := pool.QueryRow(ctx, `
		SELECT object_keys, not_before
		FROM artwork_revision_gc_candidates
		WHERE original_path = $1`, oldPath).Scan(&objectKeys, &notBefore); err != nil {
		t.Fatalf("load displaced candidate: %v", err)
	}
	if !slices.Equal(objectKeys, wantKeys) {
		t.Fatalf("object manifest = %v, want %v", objectKeys, wantKeys)
	}
	if !notBefore.After(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("not before = %v, want displacement grace period", notBefore)
	}
}
