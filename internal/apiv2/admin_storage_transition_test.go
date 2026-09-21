package apiv2

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/storagetransition"
)

type fakeAdminStorageTransition struct {
	health storagetransition.SourceHealth
	probes []bool
}

func (f *fakeAdminStorageTransition) Start(context.Context, int, storagetransition.StartRequest) (*models.AdminJob, storagetransition.Preflight, error) {
	return nil, storagetransition.Preflight{}, nil
}

func (f *fakeAdminStorageTransition) SourceHealth(_ context.Context, probe bool) (storagetransition.SourceHealth, error) {
	f.probes = append(f.probes, probe)
	return f.health, nil
}

func TestAdminStorageTransitionHealthProjectsRecoveryState(t *testing.T) {
	service := &fakeAdminStorageTransition{health: storagetransition.SourceHealth{
		CurrentBackend: "s3", Reachable: true, PublicConfigured: true, PublicReachable: true,
		PrivateReachable: true, RecoveryPending: true, RecoveryState: "waiting_retry",
		RecoveryError: "temporary storage outage", RecoveryProgress: 37, RecoveryMessage: "Waiting to retry",
	}}
	deps := requestDeps(fixtureRequests())
	deps.AdminStorageTransition = service
	handler := NewHandler(deps)
	response := do(t, handler, http.MethodGet, Prefix+"/admin/storage-transitions/source-health", "", actingRequestAdmin)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	if len(service.probes) != 1 || !service.probes[0] {
		t.Fatalf("default health request probes = %#v, want [true]", service.probes)
	}
	var body AdminStorageTransitionSourceHealth
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.RecoveryPending || body.RecoveryState != "waiting_retry" || body.RecoveryError == "" || body.RecoveryProgress != 37 || body.RecoveryMessage != "Waiting to retry" {
		t.Fatalf("recovery health = %#v", body)
	}
	service.health = storagetransition.SourceHealth{CurrentBackend: "s3", Reachable: true, PublicConfigured: true, PublicReachable: true, PrivateReachable: true}
	response = do(t, handler, http.MethodGet, Prefix+"/admin/storage-transitions/source-health", "", actingRequestAdmin)
	if response.Code != http.StatusOK || response.Body.String() == "" {
		t.Fatal(response.Code, response.Body.String())
	}
	body = AdminStorageTransitionSourceHealth{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RecoveryPending {
		t.Fatalf("cleared recovery still pending: %#v", body)
	}
}

func TestAdminStorageTransitionHealthCanSkipReachabilityProbe(t *testing.T) {
	service := &fakeAdminStorageTransition{health: storagetransition.SourceHealth{
		CurrentBackend: "s3", PublicConfigured: true, RecoveryPending: true,
		RecoveryState: "waiting_retry", RecoveryError: "temporary outage",
	}}
	deps := requestDeps(fixtureRequests())
	deps.AdminStorageTransition = service
	response := do(t, NewHandler(deps), http.MethodGet, Prefix+"/admin/storage-transitions/source-health?probe=false", "", actingRequestAdmin)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	if len(service.probes) != 1 || service.probes[0] {
		t.Fatalf("non-probing request probes = %#v, want [false]", service.probes)
	}
	var body AdminStorageTransitionSourceHealth
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.RecoveryPending || body.RecoveryState != "waiting_retry" || body.RecoveryError == "" {
		t.Fatalf("non-probing recovery health = %#v", body)
	}
}
