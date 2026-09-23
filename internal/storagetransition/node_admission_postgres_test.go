package storagetransition

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/blobstore"
	"github.com/Silo-Server/silo-server/internal/database/pglock"
)

func TestQueuedStorageTransitionRejectsNodeThatJoinedAfterStart(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	key := time.Now().UnixNano()
	owner, err := pglock.AdmitNode(t.Context(), pool, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	sourceDir, targetDir := t.TempDir(), t.TempDir()
	source := &memoryStore{identity: "local|" + sourceDir, objects: map[string][]byte{"tmdb/a.webp": []byte("a")}}
	settings := &memorySettings{values: map[string]string{
		settingArtworkBackend:   blobstore.BackendLocal,
		settingArtworkLocalPath: sourceDir,
	}}
	service := New(nil, settings, memoryJobs{}, source, nil)
	service.SetNodeAdmission(owner)
	job, _, err := service.Start(t.Context(), 1, StartRequest{Policy: PolicyMigrateAll, Values: map[string]string{
		settingArtworkBackend:   blobstore.BackendLocal,
		settingArtworkLocalPath: targetDir,
	}})
	if err != nil || job == nil {
		t.Fatalf("Start = (%v, %v), want a queued job", job, err)
	}
	peer, err := pglock.AdmitNode(t.Context(), pool, key)
	if err != nil {
		t.Fatalf("join after Start: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close(context.Background()) })
	_, err = service.ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{Policy: PolicyMigrateAll}, func(adminjob.StorageTransitionProgress) {})
	if err == nil || !strings.Contains(err.Error(), "another write-capable API node") {
		t.Fatalf("ExecuteStorageTransition after join = %v, want admission rejection", err)
	}
	if source.lists != 0 || source.gets != 0 || settings.values[blobstore.IdentitySettingKey] != "" {
		t.Fatalf("rejected job touched source or committed: lists=%d gets=%d identity=%q", source.lists, source.gets, settings.values[blobstore.IdentitySettingKey])
	}
}

func TestStorageTransitionExcludesNodeJoinThroughCopyAndCommit(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	key := time.Now().UnixNano()
	owner, err := pglock.AdmitNode(t.Context(), pool, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })

	source := &fencedMemoryStore{memoryStore: &memoryStore{
		identity: "s3|old|public|", objects: map[string][]byte{"tmdb/a.webp": []byte("a")},
	}}
	target := &memoryStore{identity: "local|target", objects: map[string][]byte{}}
	settings := stagedLocal(t, t.TempDir())
	service := New(nil, settings, nil, source, nil)
	service.SetNodeAdmission(owner)
	service.openPublic = func(map[string]string) (blobstore.Store, error) { return target, nil }
	fenced := make(chan struct{})
	resume := make(chan struct{})
	source.onFence = func() {
		close(fenced)
		<-resume
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{Policy: PolicyMigrateAll}, func(adminjob.StorageTransitionProgress) {})
		done <- err
	}()
	select {
	case <-fenced:
	case <-time.After(5 * time.Second):
		t.Fatal("transition never reached the final copy fence")
	}
	assertJoinBlocked := func(phase string) {
		t.Helper()
		joinCtx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		defer cancel()
		joined, err := pglock.AdmitNode(joinCtx, pool, key)
		if joined != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("join %s = (%v, %v), want deadline", phase, joined, err)
		}
	}
	assertJoinBlocked("during final copy")
	close(resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute transition: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transition did not finish after final copy resumed")
	}
	var stage stagedTarget
	if err := json.Unmarshal([]byte(settings.values[StagedTargetSettingKey]), &stage); err != nil {
		t.Fatal(err)
	}
	if stage.Phase != transitionPhaseRestartPending || !source.fenced {
		t.Fatalf("committed stage/fence = %q/%t", stage.Phase, source.fenced)
	}
	assertJoinBlocked("after commit, before old process exit")
	if err := owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	joined, err := pglock.AdmitNode(t.Context(), pool, key)
	if err != nil {
		t.Fatalf("join after old process exit: %v", err)
	}
	if err := joined.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}
