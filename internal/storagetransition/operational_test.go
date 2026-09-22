package storagetransition

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/blobstore"
)

// A local root holds every blob kind under one namespace since operational
// storage became backend-neutral.
func localRootObjects() map[string][]byte {
	return map[string][]byte{
		"tmdb/movie/1/poster/a.webp":        []byte("artwork"),
		"branding/logo.webp":                []byte("logo"),
		"subtitles/7/en.srt":                []byte("subtitle"),
		"profile-avatars/1/avatar.webp":     []byte("avatar"),
		"diagnostics/1/report.tar.gz":       []byte("bundle"),
		"catalog-seeds/export/seed.json.gz": []byte("seed"),
	}
}

func stagedValues(t *testing.T, values map[string]string) *memorySettings {
	t.Helper()
	raw, err := json.Marshal(stagedTarget{Values: values})
	if err != nil {
		t.Fatal(err)
	}
	return &memorySettings{values: map[string]string{StagedTargetSettingKey: string(raw)}}
}

func assertObjects(t *testing.T, name string, store *memoryStore, present, absent []string) {
	t.Helper()
	for _, key := range present {
		if _, ok := store.objects[key]; !ok {
			t.Errorf("%s is missing %s", name, key)
		}
	}
	for _, key := range absent {
		if _, ok := store.objects[key]; ok {
			t.Errorf("%s unexpectedly holds %s", name, key)
		}
	}
}

func TestLocalToS3MigrateAllSplitsSharedRootByOwner(t *testing.T) {
	source := &memoryStore{identity: "local|/srv/silo", objects: localRootObjects()}
	target := &memoryStore{identity: "s3|https://s3|public|", objects: map[string][]byte{}}
	targetPrivate := &memoryStore{identity: "s3|https://s3|private|", objects: map[string][]byte{}}
	service := testService(stagedValues(t, map[string]string{"artwork.storage_backend": "s3", "s3.public_bucket": "public", "s3.private_bucket": "private"}), source)
	service.openPublic = func(map[string]string) (blobstore.Store, error) { return target, nil }
	service.openPrivate = func(map[string]string) blobstore.Store { return targetPrivate }

	if _, err := service.ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{Policy: PolicyMigrateAll}, func(int, int, string) {}); err != nil {
		t.Fatal(err)
	}
	assertObjects(t, "public target", target,
		[]string{"tmdb/movie/1/poster/a.webp", "branding/logo.webp", "subtitles/7/en.srt"},
		[]string{"profile-avatars/1/avatar.webp", "diagnostics/1/report.tar.gz", "catalog-seeds/export/seed.json.gz"})
	assertObjects(t, "private target", targetPrivate,
		[]string{"profile-avatars/1/avatar.webp", "diagnostics/1/report.tar.gz", "catalog-seeds/export/seed.json.gz"},
		[]string{"tmdb/movie/1/poster/a.webp", "branding/logo.webp", "subtitles/7/en.srt"})
	if target.streams == 0 || targetPrivate.streams == 0 {
		t.Fatalf("copies did not stream: public=%d private=%d", target.streams, targetPrivate.streams)
	}
}

func TestS3ToLocalMigrateAllBringsOperationalDataIntoRoot(t *testing.T) {
	source := &memoryStore{identity: "s3|https://s3|public|", objects: map[string][]byte{
		"tmdb/movie/1/poster/a.webp": []byte("artwork"),
		"subtitles/7/en.srt":         []byte("subtitle"),
	}}
	private := &memoryStore{identity: "s3|https://s3|private|", objects: map[string][]byte{
		"profile-avatars/1/avatar.webp":     []byte("avatar"),
		"diagnostics/1/report.tar.gz":       []byte("bundle"),
		"catalog-seeds/export/seed.json.gz": []byte("seed"),
	}}
	target := &memoryStore{identity: "local|/srv/silo", objects: map[string][]byte{}}
	service := New(nil, stagedValues(t, map[string]string{"artwork.storage_backend": "local", "artwork.local_path": "/srv/silo"}), nil, source, private)
	service.openPublic = func(map[string]string) (blobstore.Store, error) { return target, nil }

	if _, err := service.ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{Policy: PolicyMigrateAll}, func(int, int, string) {}); err != nil {
		t.Fatal(err)
	}
	assertObjects(t, "local target", target, []string{
		"tmdb/movie/1/poster/a.webp", "subtitles/7/en.srt",
		"profile-avatars/1/avatar.webp", "diagnostics/1/report.tar.gz", "catalog-seeds/export/seed.json.gz",
	}, nil)
}

func TestS3ToLocalPreserveUploadsLeavesArtifactsBehind(t *testing.T) {
	source := &memoryStore{identity: "s3|https://s3|public|", objects: map[string][]byte{
		"tmdb/movie/1/poster/a.webp": []byte("artwork"),
		"subtitles/7/en.srt":         []byte("subtitle"),
		"library-posters/3.webp":     []byte("poster"),
	}}
	private := &memoryStore{identity: "s3|https://s3|private|", objects: map[string][]byte{
		"profile-avatars/1/avatar.webp": []byte("avatar"),
		"diagnostics/1/report.tar.gz":   []byte("bundle"),
	}}
	target := &memoryStore{identity: "local|/srv/silo", objects: map[string][]byte{}}
	service := New(nil, stagedValues(t, map[string]string{"artwork.storage_backend": "local", "artwork.local_path": "/srv/silo"}), nil, source, private)
	service.openPublic = func(map[string]string) (blobstore.Store, error) { return target, nil }
	service.reconcile = testService(nil, source).reconcile

	if _, err := service.ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{Policy: PolicyPreserveUploads}, func(int, int, string) {}); err != nil {
		t.Fatal(err)
	}
	assertObjects(t, "local target", target,
		[]string{"subtitles/7/en.srt", "library-posters/3.webp", "profile-avatars/1/avatar.webp"},
		[]string{"tmdb/movie/1/poster/a.webp", "diagnostics/1/report.tar.gz"})
}

// A local install adding a private bucket moves only operational data; the
// artwork root and its identity stay put.
func TestLocalPrivateOnlyTransitionMovesOperationalDataToBucket(t *testing.T) {
	source := &memoryStore{identity: "local|/srv/silo", objects: localRootObjects()}
	targetPrivate := &memoryStore{identity: "s3|https://s3|private|", objects: map[string][]byte{}}
	settings := stagedValues(t, map[string]string{"artwork.storage_backend": "local", "artwork.local_path": "/srv/silo", "s3.private_bucket": "private"})
	service := testService(settings, source)
	service.openPublic = func(map[string]string) (blobstore.Store, error) { return source, nil }
	service.openPrivate = func(map[string]string) blobstore.Store { return targetPrivate }

	value, err := service.ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{Policy: PolicyMigrateAll}, func(int, int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	assertObjects(t, "private target", targetPrivate,
		[]string{"profile-avatars/1/avatar.webp", "diagnostics/1/report.tar.gz", "catalog-seeds/export/seed.json.gz"},
		[]string{"tmdb/movie/1/poster/a.webp", "subtitles/7/en.srt"})
	var committed stagedTarget
	if err := json.Unmarshal([]byte(settings.values[StagedTargetSettingKey]), &committed); err != nil {
		t.Fatal(err)
	}
	if committed.PublicReconcile || committed.BrandingReconcile {
		t.Fatalf("private-only transition scheduled public reconciliation: %#v", committed)
	}
	if result := value.(Result); result.TargetIdentity != source.Identity() {
		t.Fatalf("target identity = %q, want unchanged %q", result.TargetIdentity, source.Identity())
	}
}

func TestLocalRemovingPrivateBucketBringsDataIntoRoot(t *testing.T) {
	source := &memoryStore{identity: "local|/srv/silo", objects: map[string][]byte{"tmdb/movie/1/poster/a.webp": []byte("artwork")}}
	private := &memoryStore{identity: "s3|https://s3|private|", objects: map[string][]byte{
		"profile-avatars/1/avatar.webp": []byte("avatar"),
		"diagnostics/1/report.tar.gz":   []byte("bundle"),
	}}
	service := New(nil, stagedValues(t, map[string]string{"artwork.storage_backend": "local", "artwork.local_path": "/srv/silo"}), nil, source, private)
	service.openPublic = func(map[string]string) (blobstore.Store, error) { return source, nil }

	if _, err := service.ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{Policy: PolicyMigrateAll}, func(int, int, string) {}); err != nil {
		t.Fatal(err)
	}
	assertObjects(t, "local root", source, []string{"profile-avatars/1/avatar.webp", "diagnostics/1/report.tar.gz"}, nil)
}

// The shared local root is both the assets and the operational store. Its
// fence takes the whole semaphore, so fencing it twice would hang forever.
func TestSharedLocalRootIsFencedOnce(t *testing.T) {
	fs, err := blobstore.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := blobstore.WithMutationFence(fs)
	if err := source.Put(t.Context(), "diagnostics/1/report.tar.gz", []byte("bundle")); err != nil {
		t.Fatal(err)
	}
	target := &memoryStore{identity: "s3|https://s3|public|", objects: map[string][]byte{}}
	targetPrivate := &memoryStore{identity: "s3|https://s3|private|", objects: map[string][]byte{}}
	service := New(nil, stagedValues(t, map[string]string{"artwork.storage_backend": "s3", "s3.public_bucket": "public", "s3.private_bucket": "private"}), nil, source, nil)
	service.openPublic = func(map[string]string) (blobstore.Store, error) { return target, nil }
	service.openPrivate = func(map[string]string) blobstore.Store { return targetPrivate }

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := service.ExecuteStorageTransition(ctx, adminjob.StorageTransitionRequest{Policy: PolicyMigrateAll}, func(int, int, string) {}); err != nil {
		t.Fatalf("transition over a shared local root: %v", err)
	}
	if _, ok := targetPrivate.objects["diagnostics/1/report.tar.gz"]; !ok {
		t.Fatal("diagnostic bundle was not copied to private storage")
	}
}

func TestCommitLeavesDiagnosticsEnabledOnLocalTarget(t *testing.T) {
	source := &memoryStore{identity: "s3|https://s3|public|", objects: map[string][]byte{}}
	settings := stagedLocal(t, t.TempDir())
	if _, err := testService(settings, source).ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{Policy: PolicyFresh}, func(int, int, string) {}); err != nil {
		t.Fatal(err)
	}
	if value, ok := settings.values["diagnostics.uploads_enabled"]; ok {
		t.Fatalf("commit wrote diagnostics.uploads_enabled=%q; local storage keeps diagnostics", value)
	}
}

func TestOperationalBucketNamesLocalRoot(t *testing.T) {
	for _, tt := range []struct {
		identity, private, want string
	}{
		{"local|/srv/silo", "", blobstore.LocalBucket},
		{"local|/srv/silo", "private", "private"},
		{"s3|https://s3|public|", "private", "private"},
		{"s3|https://s3|public|", "", ""},
	} {
		if got := operationalBucket(tt.identity, tt.private); got != tt.want {
			t.Errorf("operationalBucket(%q, %q) = %q, want %q", tt.identity, tt.private, got, tt.want)
		}
	}
}

func TestStartKeepsPrivateBucketOnlyForLocalSource(t *testing.T) {
	for _, tt := range []struct {
		name        string
		current     map[string]string
		wantPrivate string
	}{
		{
			name:        "disabling S3 clears private storage",
			current:     map[string]string{"artwork.storage_backend": "s3", "s3.public_bucket": "public", "s3.private_bucket": "private"},
			wantPrivate: "",
		},
		{
			name:        "local install keeps its private bucket",
			current:     map[string]string{"artwork.storage_backend": "local", "artwork.local_path": "/srv/old", "s3.private_endpoint": "https://s3", "s3.private_bucket": "private"},
			wantPrivate: "private",
		},
		{
			name:        "local install keeps a legacy operational bucket",
			current:     map[string]string{"artwork.storage_backend": "local", "artwork.local_path": "/srv/old", "s3.operational_endpoint": "https://s3", "s3.operational_bucket": "legacy"},
			wantPrivate: "legacy",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			settings := &memorySettings{values: tt.current}
			source := &memoryStore{identity: "local|/srv/old", objects: map[string][]byte{}}
			service := New(nil, settings, memoryJobs{}, source, nil)
			if _, _, err := service.Start(t.Context(), 1, StartRequest{Policy: PolicyFresh, Values: map[string]string{
				"artwork.storage_backend": "local", "artwork.local_path": "/srv/new",
			}}); err != nil {
				t.Fatal(err)
			}
			var staged stagedTarget
			if err := json.Unmarshal([]byte(settings.values[StagedTargetSettingKey]), &staged); err != nil {
				t.Fatal(err)
			}
			if got := staged.Values[settingPrivateBucket]; got != tt.wantPrivate {
				t.Fatalf("staged private bucket = %q, want %q", got, tt.wantPrivate)
			}
		})
	}
}

func TestPreflightDescribesOperationalMoves(t *testing.T) {
	local := map[string]string{"artwork.storage_backend": "local", "artwork.local_path": "/srv/silo"}
	localPrivate := map[string]string{"artwork.storage_backend": "local", "artwork.local_path": "/srv/silo", "s3.private_bucket": "private"}
	s3 := map[string]string{"artwork.storage_backend": "s3", "s3.public_bucket": "public", "s3.private_bucket": "private"}
	for _, tt := range []struct {
		name            string
		current, target map[string]string
		policy          string
		wantDiagnostics string
		wantSubtitles   string
	}{
		{"s3 to local migrate", s3, local, PolicyMigrateAll, "copied to local disk", "copied"},
		{"local to s3 migrate", local, s3, PolicyMigrateAll, "copied to the private bucket", "copied"},
		{"local to s3 preserve", local, s3, PolicyPreserveUploads, "remain on local disk", "copied"},
		{"local adds private bucket", local, localPrivate, PolicyMigrateAll, "copied to the private bucket", "stay in their current storage"},
		{"local drops private bucket", localPrivate, local, PolicyFresh, "remain on the private bucket", "stay in their current storage"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			preflight := describe(tt.current, tt.target, tt.policy)
			if !strings.Contains(preflight.Diagnostics, tt.wantDiagnostics) {
				t.Errorf("Diagnostics = %q, want %q", preflight.Diagnostics, tt.wantDiagnostics)
			}
			if !strings.Contains(preflight.Subtitles, tt.wantSubtitles) {
				t.Errorf("Subtitles = %q, want %q", preflight.Subtitles, tt.wantSubtitles)
			}
		})
	}
}

func TestPreflightKeepsAvatarsInPlaceWhenOnlyArtworkMoves(t *testing.T) {
	current := map[string]string{"artwork.storage_backend": "s3", "s3.public_bucket": "old", "s3.private_bucket": "private"}
	target := map[string]string{"artwork.storage_backend": "s3", "s3.public_bucket": "new", "s3.private_bucket": "private"}
	preflight := describe(current, target, PolicyPreserveUploads)
	if !strings.Contains(preflight.Uploads, "profile avatars stay in their current storage") {
		t.Fatalf("Uploads = %q, want avatars to stay put", preflight.Uploads)
	}
	if !strings.Contains(preflight.Diagnostics, "stay in their current storage") {
		t.Fatalf("Diagnostics = %q, want them to stay put", preflight.Diagnostics)
	}
}
