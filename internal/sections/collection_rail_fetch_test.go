package sections

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestCollectionRailItemsToFetchPreservesLegacyBoundWithoutDefaultSort(t *testing.T) {
	items := make([]*models.LibraryCollectionItem, 100_000)

	bounded := collectionRailItemsToFetch(items, 24, false)
	if len(bounded) != 24 {
		t.Fatalf("unsorted rail selected %d items, want 24", len(bounded))
	}
	visible, total := unsortedCollectionRailResult(make([]*models.MediaItem, len(bounded)))
	if len(visible) != 24 || total != 24 {
		t.Fatalf("unsorted rail result = %d items, total %d; want 24/24", len(visible), total)
	}

	all := collectionRailItemsToFetch(items, 24, true)
	if len(all) != len(items) {
		t.Fatalf("default-sorted rail selected %d items, want full membership %d", len(all), len(items))
	}
}
