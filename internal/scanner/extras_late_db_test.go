package scanner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/contentid"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lateExtrasFixture is a movie library holding one matched movie whose
// extras/ dir is populated after the movie was first scanned.
type lateExtrasFixture struct {
	pool      *pgxpool.Pool
	folder    *models.MediaFolder
	movieID   string
	titleDir  string
	extrasDir string
	moviePath string
}

func seedLateExtrasFixture(ctx context.Context, t *testing.T) lateExtrasFixture {
	t.Helper()

	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	root := t.TempDir()
	titleDir := filepath.Join(root, "Heat (1995) {tmdb-949}")
	fx := lateExtrasFixture{
		pool:      pool,
		movieID:   fmt.Sprintf("late-extras-movie-%d", time.Now().UnixNano()),
		titleDir:  titleDir,
		extrasDir: filepath.Join(titleDir, "extras"),
		moviePath: filepath.Join(titleDir, "Heat (1995) {tmdb-949} [Bluray-1080p].mkv"),
	}
	writeTestFile(t, fx.moviePath, "movie")
	if err := os.MkdirAll(fx.extrasDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders (type, name, enabled)
		VALUES ('movies', 'Late Extras Test', true)
		RETURNING id
	`).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folder_paths (media_folder_id, path) VALUES ($1, $2)
	`, folderID, root); err != nil {
		t.Fatalf("seed folder path: %v", err)
	}
	fx.folder = &models.MediaFolder{ID: folderID, Type: "movies", Paths: []string{root}, Enabled: true}
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(ctx)
		_, _ = pool.Exec(cleanupCtx, `
			DELETE FROM media_items WHERE content_id IN (
				SELECT content_id FROM media_files WHERE media_folder_id = $1
			) OR content_id = $2`, folderID, fx.movieID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM media_folders WHERE id = $1`, folderID)
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title, status, genres, poster_path, backdrop_path, logo_path)
		VALUES ($1, 'movie', 'Heat', 'matched', '{}'::text[], '', '', '')
	`, fx.movieID); err != nil {
		t.Fatalf("seed movie item: %v", err)
	}
	fx.seedFileRow(ctx, t, fx.moviePath, fx.movieID)
	return fx
}

// seedFileRow records path as an already-scanned primary file of contentID,
// with size and mtime matching disk so the scan treats it as unchanged.
func (fx lateExtrasFixture) seedFileRow(ctx context.Context, t *testing.T, path, contentID string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(ctx, `
		INSERT INTO media_files (
			content_id, media_folder_id, file_path, file_size, file_modified_at, probe_updated_at
		)
		VALUES ($1, $2, $3, $4, $5, NOW())
	`, contentID, fx.folder.ID, path, info.Size(), normalizeFileModifiedAt(info.ModTime())); err != nil {
		t.Fatalf("seed file row %s: %v", path, err)
	}
}

// seedMisimportedExtra reproduces a late extra that an earlier scan imported
// as its own unmatched movie, keyed by the local id of its path.
func (fx lateExtrasFixture) seedMisimportedExtra(ctx context.Context, t *testing.T, path string) string {
	t.Helper()
	writeTestFile(t, path, "extra")
	localID := contentid.ForLocal(path)
	if _, err := fx.pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title, status, genres, poster_path, backdrop_path, logo_path)
		VALUES ($1, 'movie', $2, 'unmatched', '{}'::text[], '', '', '')
	`, localID, filepath.Base(path)); err != nil {
		t.Fatalf("seed misimported item: %v", err)
	}
	if _, err := fx.pool.Exec(ctx, `
		INSERT INTO media_item_libraries (content_id, media_folder_id) VALUES ($1, $2)
	`, localID, fx.folder.ID); err != nil {
		t.Fatalf("seed misimported membership: %v", err)
	}
	fx.seedFileRow(ctx, t, path, localID)
	return localID
}

func (fx lateExtrasFixture) assertExtraOfMovie(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	var contentID, extraID, parentID *string
	if err := fx.pool.QueryRow(ctx, `
		SELECT mf.content_id, mf.extra_id, me.parent_id
		FROM media_files mf
		LEFT JOIN media_extras me ON me.content_id = mf.extra_id
		WHERE mf.media_folder_id = $1 AND mf.file_path = $2
	`, fx.folder.ID, path).Scan(&contentID, &extraID, &parentID); err != nil {
		t.Fatalf("read file row %s: %v", path, err)
	}
	if contentID != nil || extraID == nil || parentID == nil || *parentID != fx.movieID {
		t.Fatalf("%s: content_id=%v extra_id=%v parent=%v, want an extra of %s",
			filepath.Base(path), deref(contentID), deref(extraID), deref(parentID), fx.movieID)
	}
}

func (fx lateExtrasFixture) assertItemGone(ctx context.Context, t *testing.T, contentID string) {
	t.Helper()
	var itemExists, membershipExists bool
	if err := fx.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM media_items WHERE content_id = $1),
		       EXISTS(SELECT 1 FROM media_item_libraries WHERE content_id = $1 AND media_folder_id = $2)
	`, contentID, fx.folder.ID).Scan(&itemExists, &membershipExists); err != nil {
		t.Fatalf("read item %s: %v", contentID, err)
	}
	if membershipExists {
		t.Fatalf("misimported item %s still listed in the library (item exists=%v)", contentID, itemExists)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// A full library scan converts extras that an earlier scan imported as their
// own unmatched movies, including several siblings at once: the extras' own
// stale rows must not make the title folder look ambiguous.
func TestFullScanConvertsMisimportedLateExtras(t *testing.T) {
	ctx := t.Context()
	fx := seedLateExtrasFixture(ctx, t)
	first := filepath.Join(fx.extrasDir, "Heat - Making Of.mkv")
	second := filepath.Join(fx.extrasDir, "Interview.mkv")
	firstID := fx.seedMisimportedExtra(ctx, t, first)
	secondID := fx.seedMisimportedExtra(ctx, t, second)

	scanner := NewScanner(NewFileRepository(fx.pool), "", nil, 1, false, 0)
	if _, err := scanner.ScanFolder(ctx, fx.folder); err != nil {
		t.Fatalf("ScanFolder: %v", err)
	}

	fx.assertExtraOfMovie(ctx, t, first)
	fx.assertExtraOfMovie(ctx, t, second)
	fx.assertItemGone(ctx, t, firstID)
	fx.assertItemGone(ctx, t, secondID)
}

// An autoscan or folder-monitor event for a late extra widens to a subtree
// scan of the extras dir itself. That walk never sees the movie file beside
// the dir, which must not demote the extra to a primary movie.
func TestExtrasDirSubtreeScanClassifiesLateExtra(t *testing.T) {
	ctx := t.Context()
	fx := seedLateExtrasFixture(ctx, t)
	extra := filepath.Join(fx.extrasDir, "Heat - Making Of.mkv")
	writeTestFile(t, extra, "extra")

	scanner := NewScanner(NewFileRepository(fx.pool), "", nil, 1, false, 0)
	if _, err := scanner.ScanSubtree(ctx, fx.folder, fx.extrasDir); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}

	fx.assertExtraOfMovie(ctx, t, extra)
}

// A subtree scan of the extras dir also repairs extras an earlier scan
// imported as movies.
func TestExtrasDirSubtreeScanConvertsMisimportedExtras(t *testing.T) {
	ctx := t.Context()
	fx := seedLateExtrasFixture(ctx, t)
	first := filepath.Join(fx.extrasDir, "Making Of.mkv")
	second := filepath.Join(fx.extrasDir, "Interview.mkv")
	firstID := fx.seedMisimportedExtra(ctx, t, first)
	secondID := fx.seedMisimportedExtra(ctx, t, second)

	scanner := NewScanner(NewFileRepository(fx.pool), "", nil, 1, false, 0)
	if _, err := scanner.ScanSubtree(ctx, fx.folder, fx.extrasDir); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}

	fx.assertExtraOfMovie(ctx, t, first)
	fx.assertExtraOfMovie(ctx, t, second)
	fx.assertItemGone(ctx, t, firstID)
	fx.assertItemGone(ctx, t, secondID)
}
