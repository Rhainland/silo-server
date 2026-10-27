package migrations

import "testing"

const audiobookRuntimeFromFilesMigration = "20261027163759_audiobook_runtime_from_files"

// The backfill gives existing audiobooks the runtime their active files add
// up to, replacing a provider's value for some other edition, and leaves
// everything it has no file duration for alone.
func TestAudiobookRuntimeFromFilesBackfillPostgres(t *testing.T) {
	tx, schema := adminMigrationFixture(t)
	migrationExec(t, tx, `
CREATE TABLE media_items (content_id text PRIMARY KEY, type text, runtime integer, updated_at timestamptz);
CREATE TABLE audiobook_item_file_stats (media_folder_id integer, content_id text, duration_seconds bigint, active_file_count integer);
INSERT INTO media_items VALUES
	('provider', 'audiobook', 481, NULL),
	('long', 'audiobook', 0, NULL),
	('two-folders', 'audiobook', 0, NULL),
	('short', 'audiobook', 0, NULL),
	('unprobed', 'audiobook', 42, NULL),
	('no-files', 'audiobook', 42, NULL),
	('movie', 'movie', 120, NULL);
INSERT INTO audiobook_item_file_stats VALUES
	(1, 'provider', 60, 1),
	(1, 'long', 36000, 1),
	(1, 'two-folders', 1500, 10),
	(2, 'two-folders', 1529, 10),
	(1, 'short', 20, 1),
	(1, 'unprobed', 0, 2),
	(1, 'movie', 60, 1);`)
	migrationExec(t, tx, adminMigrationSQL(t, audiobookRuntimeFromFilesMigration, schema, false))

	for id, want := range map[string]int{
		"provider":    1,
		"long":        600,
		"two-folders": 25,
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
