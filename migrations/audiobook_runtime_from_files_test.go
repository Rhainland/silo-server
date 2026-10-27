package migrations

import "testing"

const audiobookRuntimeFromFilesMigration = "20261027163759_audiobook_runtime_from_files"

// The backfill gives existing audiobooks the runtime one copy's active files
// add up to, replacing a provider's value for some other edition, and leaves
// everything it has no file duration for alone. Two copies of a recording in
// one library are one book, not a book twice as long.
func TestAudiobookRuntimeFromFilesBackfillPostgres(t *testing.T) {
	tx, schema := adminMigrationFixture(t)
	migrationExec(t, tx, `
CREATE TABLE media_items (content_id text PRIMARY KEY, type text, runtime integer, updated_at timestamptz);
CREATE TABLE media_files (content_id text, media_folder_id integer, canonical_root_path text, duration integer, missing_since timestamptz);
INSERT INTO media_items VALUES
	('provider', 'audiobook', 481, NULL),
	('long', 'audiobook', 0, NULL),
	('two-folders', 'audiobook', 0, NULL),
	('two-copies', 'audiobook', 0, NULL),
	('missing', 'audiobook', 42, NULL),
	('short', 'audiobook', 0, NULL),
	('unprobed', 'audiobook', 42, NULL),
	('no-files', 'audiobook', 42, NULL),
	('movie', 'movie', 120, NULL);
INSERT INTO media_files VALUES
	('provider', 1, '/a/provider', 60, NULL),
	('long', 1, '/a/long', 36000, NULL),
	('two-folders', 1, '/a/tf', 150, NULL), ('two-folders', 1, '/a/tf', 1350, NULL),
	('two-folders', 2, '/b/tf', 1529, NULL),
	('two-copies', 1, '/a/m4b', 36000, NULL),
	('two-copies', 1, '/a/mp3', 36000, NULL),
	('short', 1, '/a/short', 20, NULL),
	('unprobed', 1, '/a/u', 0, NULL), ('unprobed', 1, '/a/u', 0, NULL),
	('missing', 1, '/a/gone', 900, NOW()),
	('movie', 1, '/a/movie', 60, NULL);`)
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
	} {
		var got int
		if err := tx.QueryRow(t.Context(), "SELECT runtime FROM media_items WHERE content_id = $1", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: runtime = %d, want %d", id, got, want)
		}
	}
}
