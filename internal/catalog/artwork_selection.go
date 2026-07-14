package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/artworkkey"
)

const (
	defaultArtworkGCGracePeriod = 24 * time.Hour
	artworkTargetMovie          = "movie"
	artworkTargetSeries         = "series"
	artworkTargetSeason         = "season"
	artworkTargetEpisode        = "episode"
	artworkImagePoster          = "poster"
	artworkImageBackdrop        = "backdrop"
	artworkImageLogo            = "logo"
	artworkImageStill           = "still"
	artworkPosterPathColumn     = "poster_path"
	artworkBackdropPathColumn   = "backdrop_path"
	artworkLogoPathColumn       = "logo_path"
)

// ArtworkRevisionTracker registers every immutable artwork upload before its
// objects are written, allowing the collector to reclaim uploads that are
// never published. Confirmed live revisions remain dormant in the registry so
// database triggers can later reactivate their exact object manifests.
type ArtworkRevisionTracker struct {
	pool        *pgxpool.Pool
	gracePeriod time.Duration
}

func NewArtworkRevisionTracker(pool *pgxpool.Pool) *ArtworkRevisionTracker {
	if pool == nil {
		return nil
	}
	return &ArtworkRevisionTracker{pool: pool, gracePeriod: defaultArtworkGCGracePeriod}
}

// TrackArtworkRevision refreshes the publication grace period for a revision.
// Image caching calls this before uploading, so it serializes with a collector
// that may already be deleting an older, currently-unreferenced copy.
func (t *ArtworkRevisionTracker) TrackArtworkRevision(ctx context.Context, originalPath string, objectKeys []string) error {
	if t == nil || t.pool == nil {
		return fmt.Errorf("catalog: artwork revision tracking is not configured")
	}
	return trackArtworkRevision(ctx, t.pool, originalPath, objectKeys, time.Now().Add(t.gracePeriod), true)
}

// ArtworkSelection describes a manually selected, already-cached artwork
// revision. PublishArtworkSelection makes the database pointer and image lock
// visible atomically, then schedules the displaced revision for delayed cleanup.
type ArtworkSelection struct {
	TargetType      string
	TargetContentID string
	ParentContentID string
	ImageType       string
	StoredPath      string
	SourcePath      string
	Thumbhash       string
	LockField       int
}

// ArtworkSelectionResult reports the revision displaced by a successful
// publication. PreviousPath is empty when the target had no prior artwork.
type ArtworkSelectionResult struct {
	PreviousPath string
}

// PublishArtworkSelection atomically changes the selected artwork, records its
// source, locks automatic image refreshes, and queues the old immutable revision
// for reference-aware garbage collection after a grace period.
func (s *DetailService) PublishArtworkSelection(ctx context.Context, selection ArtworkSelection) (*ArtworkSelectionResult, error) {
	if s == nil || s.itemRepo == nil || s.itemRepo.pool == nil {
		return nil, fmt.Errorf("catalog: artwork persistence is not configured")
	}
	selection.TargetType = strings.ToLower(strings.TrimSpace(selection.TargetType))
	selection.ImageType = strings.ToLower(strings.TrimSpace(selection.ImageType))
	selection.TargetContentID = strings.TrimSpace(selection.TargetContentID)
	selection.ParentContentID = strings.TrimSpace(selection.ParentContentID)
	selection.StoredPath = strings.TrimSpace(selection.StoredPath)
	if selection.TargetContentID == "" || selection.StoredPath == "" {
		return nil, fmt.Errorf("catalog: artwork target and stored path are required")
	}

	table, pathColumn, sourceColumn, thumbhashColumn, notFound, err := artworkTargetColumns(selection.TargetType, selection.ImageType)
	if err != nil {
		return nil, err
	}

	tx, err := s.itemRepo.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalog: begin artwork publication: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var previousPath string
	selectSQL := fmt.Sprintf("SELECT COALESCE(%s, '') FROM %s WHERE content_id = $1 FOR UPDATE", pathColumn, table)
	if err := tx.QueryRow(ctx, selectSQL, selection.TargetContentID).Scan(&previousPath); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound
		}
		return nil, fmt.Errorf("catalog: lock artwork target: %w", err)
	}

	setThumbhash := ""
	args := []any{selection.StoredPath, selection.SourcePath, selection.TargetContentID}
	if thumbhashColumn != "" {
		setThumbhash = fmt.Sprintf(", %s = $4", thumbhashColumn)
		args = append(args, selection.Thumbhash)
	}
	updateSQL := fmt.Sprintf(`
		UPDATE %s
		SET %s = $1, %s = $2%s, updated_at = NOW()
		WHERE content_id = $3`, table, pathColumn, sourceColumn, setThumbhash)
	if _, err := tx.Exec(ctx, updateSQL, args...); err != nil {
		return nil, fmt.Errorf("catalog: publish artwork pointer: %w", err)
	}

	lockContentID := selection.TargetContentID
	if selection.TargetType == artworkTargetSeason || selection.TargetType == artworkTargetEpisode {
		lockContentID = selection.ParentContentID
	}
	if lockContentID == "" {
		return nil, fmt.Errorf("catalog: parent content ID is required for %s artwork", selection.TargetType)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE media_items
		SET locked_fields = CASE
				WHEN $2 = ANY(COALESCE(locked_fields, '{}'::integer[])) THEN COALESCE(locked_fields, '{}'::integer[])
				ELSE array_append(COALESCE(locked_fields, '{}'::integer[]), $2)
			END,
			updated_at = NOW()
		WHERE content_id = $1`, lockContentID, selection.LockField)
	if err != nil {
		return nil, fmt.Errorf("catalog: lock manual artwork: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrItemNotFound
	}
	// Register the selected revision in the publication transaction. The first
	// cleanup pass parks it while referenced; database triggers reactivate its
	// exact manifest if any writer later displaces or deletes it.
	if err := queueArtworkRevisionGC(ctx, tx, selection.StoredPath, selection.ImageType, time.Now().Add(defaultArtworkGCGracePeriod)); err != nil {
		return nil, err
	}

	if previousPath != "" && previousPath != selection.StoredPath {
		if err := queueArtworkRevisionGC(ctx, tx, previousPath, selection.ImageType, time.Now().Add(defaultArtworkGCGracePeriod)); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("catalog: commit artwork publication: %w", err)
	}
	return &ArtworkSelectionResult{PreviousPath: previousPath}, nil
}

// QueueArtworkRevisionGC schedules an unreferenced cached revision for cleanup.
// It is used to reclaim an upload when publication fails after object storage
// succeeded. Cleanup still verifies database references before deleting.
func (s *DetailService) QueueArtworkRevisionGC(ctx context.Context, originalPath, imageType string, notBefore time.Time) error {
	if s == nil || s.itemRepo == nil || s.itemRepo.pool == nil {
		return fmt.Errorf("catalog: artwork persistence is not configured")
	}
	return queueArtworkRevisionGC(ctx, s.itemRepo.pool, originalPath, imageType, notBefore)
}

type artworkGCExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func queueArtworkRevisionGC(ctx context.Context, db artworkGCExecer, originalPath, imageType string, notBefore time.Time) error {
	return trackArtworkRevision(ctx, db, originalPath, artworkkey.ObjectKeys(originalPath, imageType), notBefore, false)
}

func trackArtworkRevision(
	ctx context.Context,
	db artworkGCExecer,
	originalPath string,
	objectKeys []string,
	notBefore time.Time,
	replaceManifest bool,
) error {
	originalPath = strings.TrimSpace(originalPath)
	keys := compactArtworkObjectKeys(objectKeys)
	if originalPath == "" || strings.Contains(originalPath, "://") || len(keys) == 0 {
		return nil
	}
	_, err := db.Exec(ctx, `
		INSERT INTO artwork_revision_gc_candidates (
			original_path, object_keys, not_before, next_attempt_at
		) VALUES ($1, $2, $3, $3)
		ON CONFLICT (original_path) DO UPDATE SET
			object_keys = CASE WHEN $4 THEN EXCLUDED.object_keys ELSE artwork_revision_gc_candidates.object_keys END,
			not_before = EXCLUDED.not_before,
			next_attempt_at = EXCLUDED.next_attempt_at,
			attempt_count = 0,
			locked_at = NULL,
			locked_by = '',
			last_error = '',
			updated_at = NOW()`, originalPath, keys, notBefore, replaceManifest)
	if err != nil {
		return fmt.Errorf("catalog: queue artwork revision cleanup: %w", err)
	}
	return nil
}

func compactArtworkObjectKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || strings.Contains(key, "://") {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

func artworkTargetColumns(targetType, imageType string) (table, pathColumn, sourceColumn, thumbhashColumn string, notFound error, err error) {
	switch targetType {
	case artworkTargetMovie, artworkTargetSeries:
		table = "media_items"
		notFound = ErrItemNotFound
		switch imageType {
		case artworkImagePoster:
			return table, artworkPosterPathColumn, "poster_source_path", "poster_thumbhash", notFound, nil
		case artworkImageBackdrop:
			return table, artworkBackdropPathColumn, "backdrop_source_path", "backdrop_thumbhash", notFound, nil
		case artworkImageLogo:
			return table, artworkLogoPathColumn, "logo_source_path", "", notFound, nil
		}
	case artworkTargetSeason:
		if imageType == artworkImagePoster {
			return "seasons", artworkPosterPathColumn, "poster_source_path", "poster_thumbhash", ErrSeasonNotFound, nil
		}
	case artworkTargetEpisode:
		if imageType == artworkImageStill {
			return "episodes", "still_path", "still_source_path", "still_thumbhash", ErrEpisodeNotFound, nil
		}
	}
	return "", "", "", "", nil, fmt.Errorf("catalog: unsupported %s artwork type %q", targetType, imageType)
}
