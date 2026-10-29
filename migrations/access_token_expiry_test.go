package migrations

import "testing"

// The access-token lifetime moves from the old 1h seed to the 8h default. A
// value an administrator chose, the refresh-token row, and a missing row stay
// as they are.
func TestAccessTokenExpiryMigrationPostgres(t *testing.T) {
	const migration = "20261029170809_access_token_expiry_8h"
	for _, tc := range []struct {
		name   string
		stored string // "" leaves the row out
		want   string
	}{
		{"old seed", "1h", "8h"},
		{"admin value", "2h", "2h"},
		{"no row", "", "no row"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, schema := adminMigrationFixture(t)
			migrationExec(t, tx, `CREATE TABLE server_settings (key text PRIMARY KEY, value text NOT NULL);
INSERT INTO server_settings (key, value) VALUES ('auth.refresh_token_expiry', '30d');`)
			if tc.stored != "" {
				if _, err := tx.Exec(t.Context(), `INSERT INTO server_settings (key, value) VALUES ('auth.access_token_expiry', $1)`, tc.stored); err != nil {
					t.Fatal(err)
				}
			}
			migrationExec(t, tx, adminMigrationSQL(t, migration, schema, false))
			var access, refresh string
			if err := tx.QueryRow(t.Context(), `SELECT
    COALESCE((SELECT value FROM server_settings WHERE key = 'auth.access_token_expiry'), 'no row'),
    (SELECT value FROM server_settings WHERE key = 'auth.refresh_token_expiry')`).Scan(&access, &refresh); err != nil {
				t.Fatal(err)
			}
			if access != tc.want || refresh != "30d" {
				t.Errorf("access_token_expiry = %q, refresh_token_expiry = %q; want %q and 30d", access, refresh, tc.want)
			}
		})
	}
}
