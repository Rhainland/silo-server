package migrations

import "testing"

const audiobookRuntimeFromFilesMigration = "20261028150609_audiobook_runtime_from_files"

// The backfill gives existing audiobooks the runtime one copy's active files
// add up to, replacing a provider's value for some other edition, and leaves
// everything it has no file duration for alone. Two copies of a recording in
// one library are one book, not a book twice as long.
func TestAudiobookRuntimeFromFilesBackfillPostgres(t *testing.T) {
	tx, schema := adminMigrationFixture(t)
	migrationExec(t, tx, `
CREATE TABLE media_items (content_id text PRIMARY KEY, type text, runtime integer, updated_at timestamptz, locked_fields integer[] DEFAULT '{}');
CREATE TABLE media_files (id serial PRIMARY KEY, content_id text, media_folder_id integer, canonical_root_path text, duration integer, missing_since timestamptz);
INSERT INTO media_items (content_id,type,runtime,updated_at) VALUES
	('provider', 'audiobook', 481, NULL),
	('long', 'audiobook', 0, NULL),
	('two-folders', 'audiobook', 0, NULL),
	('two-copies', 'audiobook', 0, NULL),
	('missing', 'audiobook', 42, NULL),
	('short', 'audiobook', 0, NULL),
	('unprobed', 'audiobook', 42, NULL),
	('no-files', 'audiobook', 42, NULL),
	('movie', 'movie', 120, NULL),
	('locked', 'audiobook', 42, NULL);
UPDATE media_items SET locked_fields='{7}' WHERE content_id='locked';
INSERT INTO media_files (content_id,media_folder_id,canonical_root_path,duration,missing_since) VALUES
	('provider', 1, '/a/provider', 60, NULL),
	('long', 1, '/a/long', 36000, NULL),
	('two-folders', 1, '/a/tf', 150, NULL), ('two-folders', 1, '/a/tf', 1350, NULL),
	('two-folders', 2, '/b/tf', 1529, NULL),
	('two-copies', 1, '/a/m4b', 36000, NULL),
	('two-copies', 1, '/a/mp3', 36000, NULL),
	('short', 1, '/a/short', 20, NULL),
	('unprobed', 1, '/a/u', 0, NULL), ('unprobed', 1, '/a/u', 0, NULL),
	('missing', 1, '/a/gone', 900, NOW()),
	('movie', 1, '/a/movie', 60, NULL),
	('locked', 1, '/a/locked', 60, NULL);`)
	migrationExec(t, tx, adminMigrationSQL(t, audiobookRuntimeFromFilesMigration, schema, false))

	for id, want := range map[string]int{
		"provider":    1,
		"long":        600,
		"two-folders": 25,
		"two-copies":  600,
		"missing":     42,
		"short":       1,
		"unprobed":    42,
		"no-files":    42,
		"movie":       120,
		"locked":      42,
	} {
		var got int
		if err := tx.QueryRow(t.Context(), "SELECT runtime FROM media_items WHERE content_id = $1", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: runtime = %d, want %d", id, got, want)
		}
	}

	// Reassigning a copy must refresh both books, including the book that
	// lost its longest copy. Losing the last playable copy keeps its value.
	migrationExec(t, tx, `
UPDATE media_files SET duration=18000 WHERE content_id='two-copies' AND canonical_root_path='/a/mp3';
UPDATE media_files SET content_id='provider' WHERE content_id='two-copies' AND canonical_root_path='/a/m4b';`)
	for id, want := range map[string]int{"two-copies": 300, "provider": 600} {
		var got int
		if err := tx.QueryRow(t.Context(), "SELECT runtime FROM media_items WHERE content_id=$1", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("after reassignment %s: runtime = %d, want %d", id, got, want)
		}
	}
	migrationExec(t, tx, `UPDATE media_files SET missing_since=NOW() WHERE content_id='two-copies'`)
	var retained int
	if err := tx.QueryRow(t.Context(), "SELECT runtime FROM media_items WHERE content_id='two-copies'").Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 300 {
		t.Errorf("all copies missing: runtime = %d, want 300", retained)
	}
}
