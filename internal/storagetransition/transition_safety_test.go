package storagetransition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/blobstore"
	"github.com/Silo-Server/silo-server/internal/models"
)

type lostAdmissionResponseJobs struct {
	job *models.AdminJob
}

func TestRecoveryCleanupPreservesNewerStage(t *testing.T) {
	settings := &memorySettings{values: map[string]string{StagedTargetSettingKey: `{"id":"new-transition","phase":"staged"}`}}
	service := New(nil, settings, nil, nil, nil)
	if err := service.clearRecovery(t.Context(), "old-transition"); err != nil {
		t.Fatal(err)
	}
	if settings.values[StagedTargetSettingKey] == "" {
		t.Fatal("late recovery cleanup erased a newer transition")
	}
}

func (j *lostAdmissionResponseJobs) GetActiveByType(context.Context, string) (*models.AdminJob, error) {
	if j.job != nil {
		return j.job, nil
	}
	return nil, adminjob.ErrJobNotFound
}

func (j *lostAdmissionResponseJobs) Create(_ context.Context, input adminjob.CreateJobInput) (*models.AdminJob, error) {
	payload, err := json.Marshal(input.RequestPayload)
	if err != nil {
		return nil, err
	}
	j.job = &models.AdminJob{ID: "accepted", JobType: input.JobType, Status: adminjob.StatusQueued, RequestPayload: payload}
	return nil, errors.New("connection lost after INSERT committed")
}

func TestStartRetainsStageWhenAdmissionResponseIsLost(t *testing.T) {
	source := &memoryStore{identity: "local|source", objects: map[string][]byte{}}
	settings := &memorySettings{values: map[string]string{settingArtworkBackend: blobstore.BackendLocal, settingArtworkLocalPath: t.TempDir()}}
	jobs := &lostAdmissionResponseJobs{}
	service := New(nil, settings, jobs, source, nil)
	_, _, err := service.Start(t.Context(), 1, StartRequest{Policy: PolicyFresh, Values: map[string]string{settingArtworkLocalPath: t.TempDir()}})
	if err == nil {
		t.Fatal("expected the lost admission response")
	}
	var request adminjob.StorageTransitionRequest
	if err := json.Unmarshal(jobs.job.RequestPayload, &request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteStorageTransition(t.Context(), request, func(int, int, string) {}); err != nil {
		t.Fatalf("accepted job cannot execute after its response was lost: %v", err)
	}
}

func TestCopyRejectsDestinationsOverlappingOppositeSource(t *testing.T) {
	for _, overlap := range []string{"public target", "private target"} {
		t.Run(overlap, func(t *testing.T) {
			publicSource := &memoryStore{identity: "s3|endpoint|public-old|", objects: map[string][]byte{"branding/logo.webp": []byte("public")}}
			privateSource := &memoryStore{identity: "s3|endpoint|private-old|", objects: map[string][]byte{"profile-avatars/u/avatar.webp": []byte("private")}}
			publicTarget := &memoryStore{identity: "s3|endpoint|public-new|", objects: map[string][]byte{}}
			privateTarget := &memoryStore{identity: "s3|endpoint|private-new|", objects: map[string][]byte{}}
			if overlap == "public target" {
				publicTarget = privateSource
			} else {
				privateTarget = publicSource
			}
			stage := stagedTarget{ID: "cross-source", Policy: PolicyMigrateAll, SourceIdentity: publicSource.Identity(), Phase: transitionPhaseStaged, Values: map[string]string{settingArtworkBackend: blobstore.BackendS3, settingPrivateBucket: "private-new"}}
			raw, err := json.Marshal(stage)
			if err != nil {
				t.Fatal(err)
			}
			settings := &memorySettings{values: map[string]string{StagedTargetSettingKey: string(raw)}}
			service := New(nil, settings, nil, publicSource, privateSource)
			service.openPublic = func(map[string]string) (blobstore.Store, error) { return publicTarget, nil }
			service.openPrivate = func(map[string]string) blobstore.Store { return privateTarget }
			_, err = service.ExecuteStorageTransition(t.Context(), adminjob.StorageTransitionRequest{TransitionID: stage.ID, Policy: stage.Policy}, func(int, int, string) {})
			if err == nil || !strings.Contains(err.Error(), "overlap") {
				t.Fatalf("expected overlap rejection before copying, got %v; public source=%v private source=%v", err, publicSource.objects, privateSource.objects)
			}
			if publicSource.puts != 0 || privateSource.puts != 0 || publicSource.lists != 0 || privateSource.lists != 0 {
				t.Fatal("copy touched a source namespace before rejecting the overlap")
			}
		})
	}
}

func TestLegacySharedSourceKeepsOperationalArtifactsPrivate(t *testing.T) {
	shared := &memoryStore{identity: "s3|endpoint|legacy|", objects: map[string][]byte{
		"tmdb/poster.webp":              []byte("poster"),
		"profile-avatars/u/avatar.webp": []byte("avatar"),
		"diagnostics/u/report.tar.gz":   []byte("diagnostic"),
		"catalog-seeds/export.json.gz":  []byte("catalog"),
	}}
	publicTarget := &memoryStore{identity: "s3|endpoint|public-new|", objects: map[string][]byte{}}
	privateTarget := &memoryStore{identity: "s3|endpoint|private-new|", objects: map[string][]byte{}}
	stage := stagedTarget{ID: "legacy-private", Policy: PolicyMigrateAll, SourceIdentity: shared.Identity(), Values: map[string]string{settingArtworkBackend: blobstore.BackendS3}}
	service := New(nil, &memorySettings{values: map[string]string{}}, nil, shared, shared)
	_, err := service.copyTransitionData(t.Context(), stage, PolicyMigrateAll, publicTarget, privateTarget, true, true, true, "run", nil, false, func(int, int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"diagnostics/u/report.tar.gz", "catalog-seeds/export.json.gz"} {
		if _, ok := publicTarget.objects[key]; ok {
			t.Errorf("private artifact %q was copied to public storage", key)
		}
		if string(privateTarget.objects[key]) != string(shared.objects[key]) {
			t.Errorf("private artifact %q is missing from the new private store", key)
		}
	}
}

func TestNestedSourceNamespacesKeepPrivateObjectsOutOfPublicCopy(t *testing.T) {
	for _, privateNested := range []bool{true, false} {
		t.Run(fmt.Sprint("private_nested=", privateNested), func(t *testing.T) {
			publicSource := &memoryStore{identity: "s3|endpoint|shared|", objects: map[string][]byte{"tmdb/poster.webp": []byte("poster")}}
			privateSource := &memoryStore{identity: "s3|endpoint|shared|private", objects: map[string][]byte{"diagnostics/report.zip": []byte("report")}}
			if privateNested {
				publicSource.objects["private/diagnostics/report.zip"] = []byte("report")
			} else {
				publicSource.identity = "s3|endpoint|shared|public"
				privateSource.identity = "s3|endpoint|shared|"
				privateSource.objects["public/tmdb/poster.webp"] = []byte("poster")
			}
			publicTarget := &memoryStore{identity: "s3|endpoint|new-public|", objects: map[string][]byte{}}
			privateTarget := &memoryStore{identity: "s3|endpoint|new-private|", objects: map[string][]byte{}}
			stage := stagedTarget{ID: "nested", SourceIdentity: publicSource.Identity(), Values: map[string]string{settingArtworkBackend: blobstore.BackendS3}}
			service := New(nil, &memorySettings{values: map[string]string{}}, nil, publicSource, privateSource)
			_, err := service.copyTransitionData(t.Context(), stage, PolicyMigrateAll, publicTarget, privateTarget, true, true, true, "run", nil, false, func(int, int, string) {})
			if err != nil {
				t.Fatal(err)
			}
			if len(publicTarget.objects) != 1 || string(publicTarget.objects["tmdb/poster.webp"]) != "poster" {
				t.Fatalf("public target includes private data: %v", publicTarget.objects)
			}
			if len(privateTarget.objects) != 1 || string(privateTarget.objects["diagnostics/report.zip"]) != "report" {
				t.Fatalf("private target contains the wrong objects: %v", privateTarget.objects)
			}
		})
	}
}

func TestPreserveUploadsExcludesNestedPrivateNamespace(t *testing.T) {
	publicSource := &memoryStore{identity: "s3|endpoint|shared|", objects: map[string][]byte{
		"branding/logo.webp":                      []byte("logo"),
		"branding/private/diagnostics/report.zip": []byte("report"),
	}}
	privateSource := &memoryStore{identity: "s3|endpoint|shared|branding/private", objects: map[string][]byte{"diagnostics/report.zip": []byte("report")}}
	publicTarget := &memoryStore{identity: "s3|endpoint|new-public|", objects: map[string][]byte{}}
	privateTarget := &memoryStore{identity: "s3|endpoint|new-private|", objects: map[string][]byte{}}
	stage := stagedTarget{ID: "nested-preserve", SourceIdentity: publicSource.Identity(), Values: map[string]string{settingArtworkBackend: blobstore.BackendS3}}
	service := New(nil, &memorySettings{values: map[string]string{}}, nil, publicSource, privateSource)
	_, err := service.copyTransitionData(t.Context(), stage, PolicyPreserveUploads, publicTarget, privateTarget, true, true, true, "run", nil, false, func(int, int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(publicTarget.objects) != 1 || string(publicTarget.objects["branding/logo.webp"]) != "logo" {
		t.Fatalf("public target includes private data: %v", publicTarget.objects)
	}
}
