package metadata

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/contentid"
	"github.com/Silo-Server/silo-server/internal/models"
)

// An Identify that corrects a series matched to the wrong show moves it to the
// right show's id right away, taking the season and episode ids composed from
// the wrong anchor along, so the wrong show can be scanned in as its own item.
func TestIdentify_CorrectedSeriesMovesWithItsChildren(t *testing.T) {
	pool := chainBuiltinTestPool(t)
	ctx := t.Context()
	items := catalog.NewItemRepository(pool)
	providerIDs := catalog.NewProviderIDRepository(pool)
	episodes := catalog.NewEpisodeRepository(pool)
	seasons := catalog.NewSeasonRepository(pool)
	service := NewMetadataService(nil, nil, nil, items, providerIDs, episodes, seasons,
		catalog.NewLibraryItemRepository(pool), catalog.NewFolderRepository(pool),
		nil, nil, nil, nil, nil)

	nonce := time.Now().UnixNano() % 100_000_000
	wrongTVDB := fmt.Sprintf("%d", 600_000_000+nonce)
	rightTVDB := fmt.Sprintf("%d", 700_000_000+nonce)
	decoyTVDB := fmt.Sprintf("%d", 800_000_000+nonce)
	from, to, decoy := "series-tvdb-"+wrongTVDB, "series-tvdb-"+rightTVDB, "series-tvdb-"+decoyTVDB
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id = ANY($1)`, []string{from, to, decoy})
	})
	mustID := func(id string, ok bool) string {
		t.Helper()
		if !ok {
			t.Fatal("content id did not compose")
		}
		return id
	}
	seedSeries := func(id, tvdb string) {
		t.Helper()
		if err := items.Upsert(ctx, &models.MediaItem{
			ContentID: id, Type: "series", Title: "Wrong show", Status: "matched", TvdbID: tvdb,
			DefaultMetadataLanguage: "en",
			Studios:                 []string{}, Networks: []string{}, Countries: []string{}, Genres: []string{},
		}); err != nil {
			t.Fatal(err)
		}
		if err := providerIDs.ReplaceByContentID(ctx, id, map[string]string{"tvdb": tvdb}); err != nil {
			t.Fatal(err)
		}
	}
	seedEpisode := func(seriesID, id string, season, episode int) {
		t.Helper()
		if err := episodes.Upsert(ctx, &models.Episode{
			ContentID: id, SeriesID: seriesID, SeasonNumber: season, EpisodeNumber: episode,
			Title: fmt.Sprintf("Episode %d", episode), DefaultMetadataLanguage: "en", MetadataSource: "provider",
		}); err != nil {
			t.Fatal(err)
		}
	}

	seedSeries(from, wrongTVDB)
	oldSeason := mustID(contentid.ForSeason(from, 1))
	if err := seasons.Upsert(ctx, &models.Season{
		ContentID: oldSeason, SeriesID: from, SeasonNumber: 1, DefaultMetadataLanguage: "en", MetadataSource: "provider",
	}); err != nil {
		t.Fatal(err)
	}
	oldEpisode1 := mustID(contentid.ForEpisode(from, 1, 1))
	oldEpisode2 := mustID(contentid.ForEpisode(from, 1, 2))
	sonyflakeEpisode := fmt.Sprintf("1460%014d", nonce)
	seedEpisode(from, oldEpisode1, 1, 1)
	seedEpisode(from, oldEpisode2, 1, 2)
	seedEpisode(from, sonyflakeEpisode, 1, 3)
	// Another row already holds the id episode 2 would move to, so episode 2
	// keeps its old id and the rest of the rename goes ahead.
	takenEpisode2 := mustID(contentid.ForEpisode(to, 1, 2))
	seedSeries(decoy, decoyTVDB)
	seedEpisode(decoy, takenEpisode2, 7, 7)

	remote := &remoteStubProvider{
		slug:     "tvdb",
		metadata: &MetadataResult{HasMetadata: true, Title: "Right show", ProviderIDs: map[string]string{"tvdb": rightTVDB}},
	}
	result, err := service.ProcessWithProviders(ctx, ProcessRequest{
		ContentID: from, ProviderIDs: map[string]string{"tvdb": rightTVDB}, Language: "en", Mode: ModeIdentify,
	}, []Provider{remote})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.ContentID != to {
		t.Fatalf("identify result = %#v, want content id %s", result, to)
	}
	if _, err := items.GetByID(ctx, from); err == nil {
		t.Errorf("series %s still exists after the correction", from)
	}

	seriesOf := func(table, id string) string {
		t.Helper()
		var seriesID string
		err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT series_id FROM %s WHERE content_id = $1`, table), id).Scan(&seriesID)
		if err != nil {
			return ""
		}
		return seriesID
	}
	for _, tc := range []struct{ table, id, wantSeries string }{
		{"seasons", mustID(contentid.ForSeason(to, 1)), to},
		{"seasons", oldSeason, ""},
		{"episodes", mustID(contentid.ForEpisode(to, 1, 1)), to},
		{"episodes", oldEpisode1, ""},
		{"episodes", oldEpisode2, to},
		{"episodes", takenEpisode2, decoy},
		{"episodes", sonyflakeEpisode, to},
	} {
		if got := seriesOf(tc.table, tc.id); got != tc.wantSeries {
			t.Errorf("%s %s belongs to %q, want %q", tc.table, tc.id, got, tc.wantSeries)
		}
	}
}
