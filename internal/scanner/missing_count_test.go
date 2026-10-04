package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestScanFolderCountsOnlyNewlyMissingFiles pins ScanResult.Missing to the
// files a scan newly marks missing. Files already marked by an earlier scan
// wait out the removal grace period; counting them again on every scan made
// one deleted file read as "11 missing" after a rename.
func TestScanFolderCountsOnlyNewlyMissingFiles(t *testing.T) {
	pool := newDeadRootTestPool(t)
	ctx := context.Background()
	folderID := seedDeadRootTestFolder(t, pool, "movies", "Missing Count Test")

	root := filepath.Join(t.TempDir(), "movies")
	paths := []string{
		filepath.Join(root, "Alpha (2020)", "Alpha (2020).mkv"),
		filepath.Join(root, "Beta (2021)", "Beta (2021).mkv"),
		filepath.Join(root, "Gamma (2022)", "Gamma (2022).mkv"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("fake movie payload"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	folder := &models.MediaFolder{
		ID:      folderID,
		Paths:   []string{root},
		Type:    "movies",
		Name:    "Missing Count Test",
		Enabled: true,
	}
	// Keep missing rows for the whole test so later scans still see them.
	scanner := NewScanner(NewFileRepository(pool), "", nil, 2, false, 24*time.Hour)

	scan := func(step string) *ScanResult {
		t.Helper()
		result, err := scanner.ScanFolder(ctx, folder)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		return result
	}
	remove := func(path string) {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove %s: %v", path, err)
		}
	}

	if got := scan("initial scan").Missing; got != 0 {
		t.Fatalf("initial scan Missing = %d, want 0", got)
	}

	remove(paths[1])
	if got := scan("scan after first delete").Missing; got != 1 {
		t.Fatalf("scan after first delete Missing = %d, want 1", got)
	}

	remove(paths[2])
	if got := scan("scan after second delete").Missing; got != 1 {
		t.Fatalf("scan after second delete Missing = %d, want 1 (the earlier file is already missing)", got)
	}

	if got := scan("unchanged rescan").Missing; got != 0 {
		t.Fatalf("unchanged rescan Missing = %d, want 0", got)
	}
}
