package migrations

import (
	"reflect"
	"testing"
)

// Before this migration, local detection never restored markers that a
// rematch or a touched file cleared, and nothing records which files lost
// them. The upgrade stamps every file missing an intro or credits range, so
// the next scheduled run looks at those seasons and movies again from their
// cached fingerprints and tails. Files holding both ranges keep no stamp and
// stay settled.
func TestTrackMarkerInvalidationBackfillPostgres(t *testing.T) {
	tx, schema := adminMigrationFixture(t)
	migrationExec(t, tx, `
CREATE TABLE media_files (id bigint PRIMARY KEY, intro_start double precision, credits_start double precision);
CREATE TABLE media_intro_fingerprints (media_file_id bigint PRIMARY KEY);
INSERT INTO media_files VALUES (1, 20, 1300), (2, NULL, 1300), (3, 20, NULL), (4, NULL, NULL);`)

	migrationExec(t, tx, adminMigrationSQL(t, "20261025203537_track_marker_invalidation", schema, false))

	rows, err := tx.Query(t.Context(), `SELECT id FROM media_files WHERE markers_invalidated_at IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var stamped []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		stamped = append(stamped, id)
	}
	if want := []int64{2, 3, 4}; !reflect.DeepEqual(stamped, want) {
		t.Fatalf("stamped files = %v, want %v", stamped, want)
	}
}
