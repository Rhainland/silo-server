package userdb

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// An existing v29 store must gain profiles.pin_revision at 0 for every profile
// it already holds, land on the current schema version, and advance the
// revision on the next PIN change.
func TestMigrateToV30AddsProfilePINRevision(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := InitSchema(db); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	store := NewSQLiteUserStore(db)
	ctx := context.Background()
	if err := store.CreateProfile(ctx, userstore.Profile{ID: "old", Name: "Old"}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE profiles DROP COLUMN pin_revision`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 29"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	version, err := userVersion(db)
	if err != nil {
		t.Fatalf("userVersion: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d, want %d", version, schemaVersion)
	}

	old, err := store.GetProfile(ctx, "old")
	if err != nil || old == nil {
		t.Fatalf("GetProfile(old): profile=%v err=%v", old, err)
	}
	if old.PINRevision != 0 {
		t.Fatalf("migrated PINRevision = %d, want 0", old.PINRevision)
	}
	pin := "1234"
	if err := store.UpdateProfile(ctx, "old", userstore.UpdateProfileInput{PIN: &pin}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if got, _ := store.GetProfile(ctx, "old"); got == nil || got.PINRevision != 1 {
		t.Fatalf("PINRevision after PIN set = %v, want 1", got)
	}
}
