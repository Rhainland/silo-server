// Package dbtest gives a test its own migrated PostgreSQL database.
//
// Package test binaries run concurrently against the one database named by
// SILO_TEST_DATABASE_URL, so a test whose subject is server-wide state (a
// singleton row such as the default access group) must not change that state
// in the shared database: another package reading it at the same moment would
// see the test's value instead of its own.
package dbtest

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/database"
	"github.com/Silo-Server/silo-server/migrations"
)

// PrivateDatabase creates a database on the server named by
// SILO_TEST_DATABASE_URL, applies every migration to it, and drops it when the
// test ends. It skips the test when SILO_TEST_DATABASE_URL is not set. The
// test role needs CREATEDB.
func PrivateDatabase(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("silo_private_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create private test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop private test database: %v", err)
		}
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect private test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := database.RunMigrations(ctx, pool, migrations.FS, "sql"); err != nil {
		t.Fatalf("migrate private test database: %v", err)
	}
	return pool
}
