package apiv2

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// registerHouseholdProbes registers one operation per shape the household
// profile gate can sit on, next to a profile-optional operation without it.
func registerHouseholdProbes(reg *Registry) {
	for _, op := range []Operation{
		{Operation: humaOp(http.MethodPost, Prefix+"/probe/household", "probeHousehold", "probe", "household-gated"), Class: ClassProfileScoped, ProfileOptional: true, HouseholdProfileGate: true},
		{Operation: humaOp(http.MethodPost, Prefix+"/probe/account", "probeAccount", "probe", "profile optional"), Class: ClassProfileScoped, ProfileOptional: true},
		{Operation: humaOp(http.MethodPost, Prefix+"/probe/household-permission", "probeHouseholdPermission", "probe", "household-gated permission"), Class: ClassPermissionGated, Permission: "marker_edit", HouseholdProfileGate: true},
	} {
		op.RetrySafety = RetrySafetyNaturalIdempotent
		Register(reg, op, probeHandler)
	}
}

func householdProbeHandler(t *testing.T, deps Dependencies) http.Handler {
	t.Helper()
	deps.testRegister = registerHouseholdProbes
	return NewHandler(deps)
}

// requireProfileHeaderProblem asserts the missing-header problem v2 answers
// on profile-required operations: 422 validation_failed at the header.
func requireProfileHeaderProblem(t *testing.T, p problemDoc) {
	t.Helper()
	if len(p.Errors) != 1 || p.Errors[0].Location != locationProfileHeader || p.Errors[0].Code != codeRequired {
		t.Fatalf("profile header error: %+v", p.Errors)
	}
}

// TestHouseholdProfileGate: an operation declaring the household profile gate
// refuses a request without X-Profile-Id, with the same problem a
// profile-required operation answers, when a profile on the account is
// PIN-protected or access-restricted. An unrestricted household, a named
// profile, an API key, and an operation without the gate keep account scope.
func TestHouseholdProfileGate(t *testing.T) {
	h := householdProbeHandler(t, parityDeps(false))
	body := `{"name":"x","cleared":null}`
	for _, tc := range []struct {
		name    string
		path    string
		headers map[string]string
		refused bool
	}{
		{"limited household without header", "/api/v2/probe/household", bearer(householdToken), true},
		// Apple sends an empty X-Profile-Id when no profile is selected; it
		// is no profile, exactly as viewer access reads it.
		{"limited household with empty header", "/api/v2/probe/household", with(bearer(householdToken), "X-Profile-Id", ""), true},
		{"limited household naming the child", "/api/v2/probe/household", with(bearer(householdToken), "X-Profile-Id", "p-kid"), false},
		{"limited household on an ungated operation", "/api/v2/probe/account", bearer(householdToken), false},
		{"unrestricted household without header", "/api/v2/probe/household", bearer(memberToken), false},
		{"api key without header", "/api/v2/probe/household", bearer(householdAPIKeyToken), false},
		{"permission operation without header", "/api/v2/probe/household-permission", bearer(householdToken), true},
		{"permission operation naming the child", "/api/v2/probe/household-permission", with(bearer(householdToken), "X-Profile-Id", "p-kid"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, tc.path, body, tc.headers)
			if !tc.refused {
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				return
			}
			requireProfileHeaderProblem(t, requireProblem(t, rec, TypeValidationFailed))
		})
	}

	// The PIN still guards a named locked profile: the household gate
	// passes a request naming one, and viewer access judges it.
	requireProblem(t, do(t, h, http.MethodPost, "/api/v2/probe/household", body, with(bearer(householdToken), "X-Profile-Id", "p-primary-locked")), TypeProfileVerificationRequired)
}

// TestHouseholdProfileGateFailsClosed: an operation declaring the gate is not
// served without it, while profile-optional operations without it are.
func TestHouseholdProfileGateFailsClosed(t *testing.T) {
	deps := parityDeps(false)
	deps.HouseholdProfile = nil
	h := householdProbeHandler(t, deps)
	body := `{"name":"x","cleared":null}`
	for _, path := range []string{"/api/v2/probe/household", "/api/v2/probe/household-permission"} {
		requireProblem(t, do(t, h, http.MethodPost, path, body, with(bearer(memberToken), "X-Profile-Id", "p-owner")), TypeDependencyUnavailable)
	}
	if rec := do(t, h, http.MethodPost, "/api/v2/probe/account", body, bearer(memberToken)); rec.Code != http.StatusOK {
		t.Fatalf("ungated operation: %d %s", rec.Code, rec.Body.String())
	}

	// parityDeps wires no generic limiter: auth, viewer access, household.
	chain, missing := gateChain(parityDeps(false), ClassProfileScoped, "", false, true, true, "")
	if missing != "" || len(chain) != 3 {
		t.Fatalf("household chain = %d gates, missing %q; want auth, viewer access, household", len(chain), missing)
	}
	chain, missing = gateChain(parityDeps(false), ClassPermissionGated, "marker_edit", false, false, true, "")
	if missing != "" || len(chain) != 4 {
		t.Fatalf("household permission chain = %d gates, missing %q; want auth, viewer access, household, permission", len(chain), missing)
	}
}

// TestHouseholdProfileGateDeclaration: the gate narrows an absent header, so
// it is refused where the header is already required or never resolved.
func TestHouseholdProfileGateDeclaration(t *testing.T) {
	base := humaOp(http.MethodGet, Prefix+"/x", "getX", "probe", "x")
	for _, op := range []Operation{
		{Operation: base, Class: ClassProfileScoped, HouseholdProfileGate: true},
		{Operation: base, Class: ClassAuthenticated, HouseholdProfileGate: true},
		{Operation: base, Class: ClassActingAdmin, HouseholdProfileGate: true},
		{Operation: base, Class: ClassPublic, HouseholdProfileGate: true},
	} {
		if err := checkOperation(op); err == nil || !strings.Contains(err.Error(), "household profile gate") {
			t.Fatalf("%s: err = %v", op.Class, err)
		}
	}
	for _, op := range []Operation{
		{Operation: base, Class: ClassProfileScoped, ProfileOptional: true, HouseholdProfileGate: true},
		{Operation: base, Class: ClassPermissionGated, Permission: "marker_edit", HouseholdProfileGate: true},
	} {
		if err := checkOperation(op); err != nil {
			t.Fatalf("%s: %v", op.Class, err)
		}
	}
}

// householdGatedOperations is every operation that runs the household profile
// gate: the viewer reads and actions that serve one media item or file. The
// capability probes, profile selection, account operations, library
// discovery, the session-bound stream routes and direct downloads stay
// account scoped without a header.
var householdGatedOperations = []string{
	"cancelSubtitleAIJob",
	"clearFileMarkerSegment",
	"createSubtitleAIJob",
	"deleteStoredSubtitle",
	"downloadSubtitle",
	"getFileMarkers",
	"getItemMarkers",
	"getStoredSubtitleSync",
	"getSubtitleAIJob",
	"getSubtitleSync",
	"getViewerSubtitleMetadata",
	"getWatchState",
	"getWatchTrickplay",
	"listStoredSubtitles",
	"listSubtitleAIJobs",
	"listSubtitleSync",
	"searchSubtitles",
	"setFileMarkers",
	"setItemMarkers",
	"setStoredSubtitleTiming",
	"setSubtitleTiming",
	"startSubtitleSync",
	"syncStoredSubtitle",
	"uploadSubtitle",
}

// TestHouseholdProfileGateDocumented: exactly the gated operations document
// X-Profile-Id with the household rule, as optional, and declare the 422 the
// gate answers.
func TestHouseholdProfileGateDocumented(t *testing.T) {
	doc := generatedDocument(t)
	var got []string
	for _, item := range doc["paths"].(map[string]any) {
		for _, raw := range item.(map[string]any) {
			op, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			params, _ := op["parameters"].([]any)
			for _, p := range params {
				param := p.(map[string]any)
				if param["in"] != "header" || param["name"] != profileHeader || param["description"] != householdProfileHeaderDescription {
					continue
				}
				id, _ := op["operationId"].(string)
				got = append(got, id)
				if param["required"] == true {
					t.Errorf("%s documents %s as required", id, profileHeader)
				}
				if _, ok := op["responses"].(map[string]any)["422"]; !ok {
					t.Errorf("%s does not document 422", id)
				}
			}
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, householdGatedOperations) {
		t.Fatalf("household-gated operations = %v\nwant %v", got, householdGatedOperations)
	}
}

// TestHouseholdProfileGateOnViewerOperations drives one real operation of
// each kind as the account of a household with a PIN-locked parent and a
// TV-Y7 child: without X-Profile-Id it is refused before the service runs;
// naming the child reaches the service.
func TestHouseholdProfileGateOnViewerOperations(t *testing.T) {
	deps, _ := catalogDeps(t)
	watch := &fakeWatch{}
	markers := &fakeMarkers{}
	subs := &fakeSubtitleReads{}
	deps.Watch, deps.Markers, deps.SubtitleReads = watch, markers, subs
	h := newTestHandler(t, deps)
	child := with(bearer(householdToken), "X-Profile-Id", "p-kid")

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"watch detail", http.MethodGet, "/api/v2/watch/movie:heat-1995", ""},
		{"item markers", http.MethodGet, "/api/v2/markers/items/movie:one", ""},
		{"file marker write", http.MethodPut, "/api/v2/markers/files/5", `{"recap":null}`},
		{"stored subtitles", http.MethodGet, "/api/v2/subtitles/42", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.path, tc.body, bearer(householdToken))
			requireProfileHeaderProblem(t, requireProblem(t, rec, TypeValidationFailed))
			if len(watch.filters) != 0 || markers.calls != 0 || subs.fileID != 0 {
				t.Fatal("refused request reached the service")
			}
		})
	}

	// An unrestricted household keeps account scope.
	if rec := do(t, h, http.MethodGet, "/api/v2/watch/movie:heat-1995", "", bearer(memberToken)); rec.Code != http.StatusOK {
		t.Fatalf("unrestricted household: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, h, http.MethodGet, "/api/v2/watch/movie:heat-1995", "", child); rec.Code != http.StatusOK {
		t.Fatalf("watch as the child: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v2/markers/items/movie:one", "", child); rec.Code != http.StatusOK || markers.calls != 1 {
		t.Fatalf("markers as the child: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPut, "/api/v2/markers/files/5", `{"recap":null}`, child); rec.Code != http.StatusOK {
		t.Fatalf("marker write as the child: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v2/subtitles/42", "", child); rec.Code != http.StatusOK || subs.access.UserID != householdUserID || subs.fileID != 42 {
		t.Fatalf("subtitles as the child: %d %s access %+v", rec.Code, rec.Body.String(), subs.access)
	}
}
