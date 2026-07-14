package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	artworkRevisionGCBatchSize = 100
	artworkRevisionGCLease     = 15 * time.Minute
)

// ArtworkRevisionDeleter is the object-storage surface used by revision GC.
type ArtworkRevisionDeleter interface {
	DeleteObjects(ctx context.Context, bucket string, keys []string) (int, error)
	Bucket() string
}

// ArtworkRevisionGCStats summarizes one bounded cleanup pass.
type ArtworkRevisionGCStats struct {
	Claimed    int `json:"claimed"`
	Deleted    int `json:"deleted"`
	Referenced int `json:"referenced"`
	Retried    int `json:"retried"`
}

// ArtworkRevisionGarbageCollector deletes unpublished or displaced immutable
// revisions only after their grace period and while no catalog surface
// references them. Work is leased with SKIP LOCKED so multiple workers are safe.
type ArtworkRevisionGarbageCollector struct {
	pool *pgxpool.Pool
	s3   ArtworkRevisionDeleter
}

func NewArtworkRevisionGarbageCollector(pool *pgxpool.Pool, s3 ArtworkRevisionDeleter) *ArtworkRevisionGarbageCollector {
	if pool == nil || s3 == nil {
		return nil
	}
	return &ArtworkRevisionGarbageCollector{pool: pool, s3: s3}
}

// Run processes one bounded batch. Failed deletions are retried with
// exponential backoff; an expired lease is recoverable by another worker.
func (g *ArtworkRevisionGarbageCollector) Run(ctx context.Context) (ArtworkRevisionGCStats, error) {
	stats := ArtworkRevisionGCStats{}
	if g == nil || g.pool == nil || g.s3 == nil {
		return stats, fmt.Errorf("artwork revision GC is not configured")
	}

	workerID := uuid.NewString()
	candidates, err := g.claim(ctx, workerID, artworkRevisionGCBatchSize)
	if err != nil {
		return stats, err
	}
	return processArtworkRevisionGCBatch(
		candidates,
		func(candidate artworkRevisionGCCandidate) (artworkRevisionGCOutcome, error) {
			return g.processCandidate(ctx, candidate, workerID)
		},
		func(candidate artworkRevisionGCCandidate, cause error) error {
			return g.retry(ctx, candidate, workerID, cause)
		},
	)
}

func processArtworkRevisionGCBatch(
	candidates []artworkRevisionGCCandidate,
	process func(artworkRevisionGCCandidate) (artworkRevisionGCOutcome, error),
	retry func(artworkRevisionGCCandidate, error) error,
) (ArtworkRevisionGCStats, error) {
	stats := ArtworkRevisionGCStats{Claimed: len(candidates)}
	var firstErr error
	for _, candidate := range candidates {
		outcome, err := process(candidate)
		if err != nil {
			stats.Retried++
			if retryErr := retry(candidate, err); retryErr != nil && firstErr == nil {
				firstErr = retryErr
			}
			continue
		}
		switch outcome {
		case artworkRevisionGCReferenced:
			stats.Referenced++
		case artworkRevisionGCDeleted:
			stats.Deleted++
		}
	}
	return stats, firstErr
}

type artworkRevisionGCOutcome int

const (
	artworkRevisionGCSuperseded artworkRevisionGCOutcome = iota
	artworkRevisionGCReferenced
	artworkRevisionGCDeleted
)

type artworkRevisionGCCandidate struct {
	id           int64
	originalPath string
	objectKeys   []string
	attemptCount int
}

func (g *ArtworkRevisionGarbageCollector) claim(ctx context.Context, workerID string, limit int) ([]artworkRevisionGCCandidate, error) {
	rows, err := g.pool.Query(ctx, `
		WITH due AS (
			SELECT id
			FROM artwork_revision_gc_candidates
			WHERE not_before <= NOW()
			  AND next_attempt_at <= NOW()
			  AND (locked_at IS NULL OR locked_at < NOW() - ($3 * interval '1 second'))
			ORDER BY next_attempt_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE artwork_revision_gc_candidates AS candidate
		SET locked_at = NOW(), locked_by = $2, updated_at = NOW()
		FROM due
		WHERE candidate.id = due.id
		RETURNING candidate.id, candidate.original_path, candidate.object_keys, candidate.attempt_count`,
		limit, workerID, int64(artworkRevisionGCLease/time.Second))
	if err != nil {
		return nil, fmt.Errorf("artwork revision GC: claim: %w", err)
	}
	defer rows.Close()

	var candidates []artworkRevisionGCCandidate
	for rows.Next() {
		var candidate artworkRevisionGCCandidate
		if err := rows.Scan(&candidate.id, &candidate.originalPath, &candidate.objectKeys, &candidate.attemptCount); err != nil {
			return nil, fmt.Errorf("artwork revision GC: scan claim: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artwork revision GC: claims: %w", err)
	}
	return candidates, nil
}

// processCandidate holds the registry row lock across the last reference check
// and object deletion. A concurrent cache attempt registers the revision before
// uploading and therefore waits here; once deletion commits, that attempt can
// safely recreate the complete object set before publication.
func (g *ArtworkRevisionGarbageCollector) processCandidate(
	ctx context.Context,
	candidate artworkRevisionGCCandidate,
	workerID string,
) (artworkRevisionGCOutcome, error) {
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return artworkRevisionGCSuperseded, fmt.Errorf("artwork revision GC: begin deletion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var originalPath string
	var objectKeys []string
	err = tx.QueryRow(ctx, `
		SELECT original_path, object_keys
		FROM artwork_revision_gc_candidates
		WHERE id = $1 AND locked_by = $2
		FOR UPDATE`, candidate.id, workerID).Scan(&originalPath, &objectKeys)
	if errors.Is(err, pgx.ErrNoRows) {
		return artworkRevisionGCSuperseded, nil
	}
	if err != nil {
		return artworkRevisionGCSuperseded, fmt.Errorf("artwork revision GC: lock candidate: %w", err)
	}

	referenced, err := g.isReferenced(ctx, tx, originalPath)
	if err != nil {
		return artworkRevisionGCSuperseded, err
	}
	if referenced {
		if _, err := tx.Exec(ctx, `
			UPDATE artwork_revision_gc_candidates
			SET next_attempt_at = NULL,
				attempt_count = 0,
				locked_at = NULL,
				locked_by = '',
				last_error = '',
				updated_at = NOW()
			WHERE id = $1 AND locked_by = $2`, candidate.id, workerID); err != nil {
			return artworkRevisionGCSuperseded, fmt.Errorf("artwork revision GC: park referenced revision: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return artworkRevisionGCSuperseded, fmt.Errorf("artwork revision GC: commit referenced revision: %w", err)
		}
		return artworkRevisionGCReferenced, nil
	}

	deleted, err := g.s3.DeleteObjects(ctx, g.s3.Bucket(), objectKeys)
	if err == nil && deleted != len(objectKeys) {
		err = fmt.Errorf("deleted %d of %d artwork objects", deleted, len(objectKeys))
	}
	if err != nil {
		return artworkRevisionGCSuperseded, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM artwork_revision_gc_candidates WHERE id = $1 AND locked_by = $2`, candidate.id, workerID); err != nil {
		return artworkRevisionGCSuperseded, fmt.Errorf("artwork revision GC: finish: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return artworkRevisionGCSuperseded, fmt.Errorf("artwork revision GC: commit deletion: %w", err)
	}
	return artworkRevisionGCDeleted, nil
}

type artworkReferenceQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (g *ArtworkRevisionGarbageCollector) isReferenced(ctx context.Context, q artworkReferenceQuerier, originalPath string) (bool, error) {
	parts := make([]string, 0, len(artworkSweepSurfaces()))
	for _, surface := range artworkSweepSurfaces() {
		parts = append(parts, fmt.Sprintf("SELECT 1 FROM %s WHERE %s = $1", surface.table, surface.pathCol))
	}
	query := "SELECT EXISTS(" + strings.Join(parts, " UNION ALL ") + ")"
	var referenced bool
	if err := q.QueryRow(ctx, query, originalPath).Scan(&referenced); err != nil {
		return false, fmt.Errorf("artwork revision GC: check references: %w", err)
	}
	return referenced, nil
}

func (g *ArtworkRevisionGarbageCollector) retry(ctx context.Context, candidate artworkRevisionGCCandidate, workerID string, cause error) error {
	delay := time.Minute << min(candidate.attemptCount, 10)
	_, err := g.pool.Exec(ctx, `
		UPDATE artwork_revision_gc_candidates
		SET attempt_count = attempt_count + 1,
			next_attempt_at = NOW() + ($3 * interval '1 second'),
			locked_at = NULL,
			locked_by = '',
			last_error = $4,
			updated_at = NOW()
		WHERE id = $1 AND locked_by = $2`, candidate.id, workerID, int64(delay/time.Second), cause.Error())
	if err != nil {
		return fmt.Errorf("artwork revision GC: schedule retry: %w", err)
	}
	return nil
}

func (s ArtworkRevisionGCStats) JSON() []byte {
	data, _ := json.Marshal(s)
	return data
}
