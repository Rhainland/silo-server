package access_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/access"
	"github.com/Silo-Server/silo-server/internal/database/dbtest"
)

// These tests move the server-wide default access group, so each runs on its
// own database: on the shared one a concurrently running package would see
// the default missing or pointing at a test group. They live in access_test
// because dbtest runs the migrations through internal/database, which imports
// this package.

func TestDefaultAccessGroupSeedAndUniqueDB(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.PrivateDatabase(t)
	store := access.NewGroupStore(pool)

	assertDefaultGroupSeed(t, ctx, pool)

	_, err := pool.Exec(ctx, `
		INSERT INTO access_groups (name, is_default)
		VALUES ('Second default', true)`)
	if err == nil {
		t.Fatal("second default access group insert succeeded, want unique violation")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("second default insert error = %v, want unique violation", err)
	}

	group := createDefaultTestGroup(t, ctx, store, "swap-default")
	isDefault := true
	updated, err := store.Update(ctx, group.ID, access.UpdateGroupInput{IsDefault: &isDefault})
	if err != nil {
		t.Fatalf("Update(is_default true) error: %v", err)
	}
	if !updated.IsDefault {
		t.Fatalf("updated IsDefault = false, want true")
	}
	assertSingleDefaultGroup(t, ctx, pool, group.ID)

	isDefault = false
	if _, err := store.Update(ctx, group.ID, access.UpdateGroupInput{IsDefault: &isDefault}); !errors.Is(err, access.ErrDefaultGroupRequired) {
		t.Fatalf("Update(is_default false) on the default group error = %v, want ErrDefaultGroupRequired", err)
	}
	assertSingleDefaultGroup(t, ctx, pool, group.ID)
}

func TestGroupStoreDeleteDefaultRejectedDB(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.PrivateDatabase(t)
	store := access.NewGroupStore(pool)

	group := createDefaultTestGroup(t, ctx, store, "delete-default")
	isDefault := true
	if _, err := store.Update(ctx, group.ID, access.UpdateGroupInput{IsDefault: &isDefault}); err != nil {
		t.Fatalf("Update(is_default true) error: %v", err)
	}
	userID := insertDefaultTestUser(t, ctx, pool, "default-member", group.ID)

	if err := store.Delete(ctx, group.ID); !errors.Is(err, access.ErrDefaultGroupRequired) {
		t.Fatalf("Delete(default) error = %v, want ErrDefaultGroupRequired", err)
	}
	var hasGroup bool
	if err := pool.QueryRow(ctx, `
		SELECT access_group_id IS NOT NULL
		FROM users
		WHERE id = $1`, userID).Scan(&hasGroup); err != nil {
		t.Fatalf("load default group member: %v", err)
	}
	if !hasGroup {
		t.Fatalf("user access_group_id cleared by rejected default-group delete")
	}
	assertSingleDefaultGroup(t, ctx, pool, group.ID)

	// Deleting a non-default group still clears memberships through the FK.
	other := createDefaultTestGroup(t, ctx, store, "delete-non-default")
	otherUserID := insertDefaultTestUser(t, ctx, pool, "non-default-member", other.ID)
	if err := store.Delete(ctx, other.ID); err != nil {
		t.Fatalf("Delete(non-default) error: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT access_group_id IS NOT NULL
		FROM users
		WHERE id = $1`, otherUserID).Scan(&hasGroup); err != nil {
		t.Fatalf("load deleted group member: %v", err)
	}
	if hasGroup {
		t.Fatalf("user access_group_id remained set after deleting non-default group")
	}
}

func createDefaultTestGroup(t *testing.T, ctx context.Context, store *access.GroupStore, name string) *access.Group {
	t.Helper()
	group, err := store.Create(ctx, access.CreateGroupInput{
		Name:       name,
		MaxStreams: 1,
	})
	if err != nil {
		t.Fatalf("Create(%s) error: %v", name, err)
	}
	return group
}

func insertDefaultTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, username string, groupID int64) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (username, role, enabled, access_group_id)
		VALUES ($1, 'user', true, $2)
		RETURNING id`,
		username,
		groupID,
	).Scan(&id); err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	return id
}

func assertDefaultGroupSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var (
		description              string
		libraryIDsNull           bool
		maxPlaybackQuality       string
		downloadAllowed          bool
		downloadTranscodeAllowed bool
		maxStreams               int
		maxTranscodes            int
		allowedPermissions       []string
		requestsAllowed          bool
	)
	if err := pool.QueryRow(ctx, `
		SELECT description, library_ids IS NULL, max_playback_quality,
			download_allowed, download_transcode_allowed, max_streams,
			max_transcodes, allowed_permissions, requests_allowed
		FROM access_groups
		WHERE name = 'Default Group'
		  AND is_default`).Scan(
		&description,
		&libraryIDsNull,
		&maxPlaybackQuality,
		&downloadAllowed,
		&downloadTranscodeAllowed,
		&maxStreams,
		&maxTranscodes,
		&allowedPermissions,
		&requestsAllowed,
	); err != nil {
		t.Fatalf("load seeded default access group details: %v", err)
	}
	if description != "Applied automatically to newly created users." ||
		!libraryIDsNull ||
		maxPlaybackQuality != "" ||
		!downloadAllowed ||
		downloadTranscodeAllowed ||
		maxStreams != 5 ||
		maxTranscodes != 5 ||
		!slices.Equal(allowedPermissions, []string{"marker_edit"}) ||
		!requestsAllowed {
		t.Fatalf("seeded default group does not match the migration's starter policy")
	}
}

func assertSingleDefaultGroup(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wantID int64) {
	t.Helper()
	var (
		gotID int64
		count int
	)
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(MIN(id), 0), COUNT(*)::int
		FROM access_groups
		WHERE is_default`).Scan(&gotID, &count); err != nil {
		t.Fatalf("count default access groups: %v", err)
	}
	if count != 1 || gotID != wantID {
		t.Fatalf("default groups = count %d id %d, want count 1 id %d", count, gotID, wantID)
	}
}
