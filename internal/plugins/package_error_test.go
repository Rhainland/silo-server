package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func packageErrorTestManifest(pluginID, version string) *pluginv1.PluginManifest {
	return &pluginv1.PluginManifest{
		PluginId:           pluginID,
		Version:            version,
		Checksum:           strings.Repeat("a", 64),
		SiloApiVersion:     DefaultSiloAPIVersion,
		SupportedPlatforms: []*pluginv1.SupportedPlatform{{Os: "linux", Arch: "amd64"}},
		Capabilities: []*pluginv1.CapabilityDescriptor{
			{Type: "metadata_provider.v1", Id: "fixture", DisplayName: "Fixture"},
		},
	}
}

func packageErrorTestServer(t *testing.T, status int, body []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func requirePackageError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("install succeeded, want a package error")
	}
	packageErr, ok := AsPackageError(err)
	if !ok {
		t.Fatalf("error %v is not a PackageError", err)
	}
	if !strings.Contains(packageErr.Message, want) {
		t.Fatalf("package error message = %q, want it to contain %q", packageErr.Message, want)
	}
}

func requireTransient(t *testing.T, err error, want bool) {
	t.Helper()
	packageErr, ok := AsPackageError(err)
	if !ok || packageErr.Transient != want {
		t.Fatalf("error %v transient = %v, want %v", err, ok && packageErr.Transient, want)
	}
}

// A download that does not hash to the catalog's checksum is refused before
// anything is written, with a reason the admin can read.
func TestInstallBinaryChecksumMismatchIsPackageError(t *testing.T) {
	server := packageErrorTestServer(t, http.StatusOK, []byte("tampered"))
	installer := NewInstaller(nil, InstallerOptions{BaseDir: t.TempDir(), HTTPClient: server.Client()})

	_, err := installer.InstallBinary(context.Background(), InstallBinaryRequest{
		BinaryURL: server.URL + "/plugin",
		Checksum:  sha256Hex([]byte("original")),
		Manifest:  packageErrorTestManifest("test.plugin", "1.0.0"),
	})
	requirePackageError(t, err, "SHA-256 checksum")
	requireTransient(t, err, false)
}

// A missing file stays missing; a host that is down or rate limiting may
// answer a retry.
func TestInstallBinaryDownloadStatusIsPackageError(t *testing.T) {
	for _, tc := range []struct {
		status    int
		want      string
		transient bool
	}{
		{status: http.StatusNotFound, want: "HTTP 404", transient: false},
		{status: http.StatusTooManyRequests, want: "HTTP 429", transient: true},
		{status: http.StatusBadGateway, want: "HTTP 502", transient: true},
	} {
		server := packageErrorTestServer(t, tc.status, nil)
		installer := NewInstaller(nil, InstallerOptions{BaseDir: t.TempDir(), HTTPClient: server.Client()})

		_, err := installer.InstallBinary(context.Background(), InstallBinaryRequest{
			BinaryURL: server.URL + "/plugin",
			Checksum:  sha256Hex(nil),
		})
		requirePackageError(t, err, tc.want)
		requireTransient(t, err, tc.transient)
	}
}

func TestInstallBinaryUnreachableHostIsTransient(t *testing.T) {
	server := packageErrorTestServer(t, http.StatusOK, nil)
	url := server.URL + "/plugin"
	server.Close()
	installer := NewInstaller(nil, InstallerOptions{BaseDir: t.TempDir()})

	_, err := installer.InstallBinary(context.Background(), InstallBinaryRequest{BinaryURL: url, Checksum: sha256Hex(nil)})
	requirePackageError(t, err, "couldn't download the plugin")
	requireTransient(t, err, true)
}

// A catalog entry must not install a binary that says it is another plugin
// or another version: the catalog's identity decides which installation an
// install replaces.
func TestInstallBinaryRejectsBinaryNotListedByCatalog(t *testing.T) {
	body := []byte("binary")
	server := packageErrorTestServer(t, http.StatusOK, body)
	installer := NewInstaller(nil, InstallerOptions{BaseDir: t.TempDir(), HTTPClient: server.Client()})

	for _, tc := range []struct {
		name     string
		manifest *pluginv1.PluginManifest
	}{
		{name: "other plugin", manifest: packageErrorTestManifest("silo.tmdb", "1.0.0")},
		{name: "other version", manifest: packageErrorTestManifest("test.plugin", "2.0.0")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := installer.InstallBinary(context.Background(), InstallBinaryRequest{
				BinaryURL: server.URL + "/plugin",
				Checksum:  sha256Hex(body),
				Manifest:  tc.manifest,
				PluginID:  "test.plugin",
				Version:   "1.0.0",
			})
			requirePackageError(t, err, "not the test.plugin 1.0.0 its catalog lists")
		})
	}
}

func TestResolvedCatalogInstallBinaryRequestCarriesCatalogIdentity(t *testing.T) {
	target := &ResolvedCatalogInstall{RepositoryID: 7, PluginID: "test.plugin", Version: "1.2.0", ArchiveURL: "https://example.test/plugin", Checksum: "abc"}
	req := target.BinaryRequest()
	if req.PluginID != "test.plugin" || req.Version != "1.2.0" || req.BinaryURL != target.ArchiveURL || req.Checksum != "abc" {
		t.Fatalf("BinaryRequest() = %+v, want the target's identity, URL and checksum", req)
	}
	if req.RepositoryID == nil || *req.RepositoryID != 7 {
		t.Fatalf("BinaryRequest().RepositoryID = %v, want 7", req.RepositoryID)
	}
}
