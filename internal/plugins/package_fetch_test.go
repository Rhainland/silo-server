package plugins

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fetchDeadline is how long a test waits for a fetch that should give up on
// its own. It only guards against a hang; the fetch fails long before it.
const fetchDeadline = 10 * time.Second

var shortPackageFetchTimeouts = packageFetchTimeouts{
	dial:           time.Second,
	tlsHandshake:   time.Second,
	responseHeader: 100 * time.Millisecond,
}

// stallingServer runs handler, which may block on release until the test ends.
func stallingServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, release <-chan struct{})) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, release)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server
}

func fetchWithDeadline(t *testing.T, fetch func() ([]byte, error)) ([]byte, error) {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := fetch()
		done <- result{data, err}
	}()
	select {
	case r := <-done:
		return r.data, r.err
	case <-time.After(fetchDeadline):
		t.Fatalf("fetch did not finish within %s", fetchDeadline)
		return nil, nil
	}
}

func TestPackageFetchDefaultClientHasTimeouts(t *testing.T) {
	for name, client := range map[string]*http.Client{
		"catalog":   NewCatalogService(nil, CatalogServiceOptions{}).httpClient,
		"installer": NewInstaller(nil, InstallerOptions{}).httpClient,
	} {
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s client transport = %T, want *http.Transport", name, client.Transport)
		}
		if transport.ResponseHeaderTimeout != packageFetchResponseHeaderTimeout ||
			transport.TLSHandshakeTimeout != packageFetchTLSHandshakeTimeout {
			t.Fatalf("%s transport timeouts = header %s, tls %s", name, transport.ResponseHeaderTimeout, transport.TLSHandshakeTimeout)
		}
		if client.Timeout != 0 {
			t.Fatalf("%s client has a whole-request timeout %s; slow downloads would be cut off", name, client.Timeout)
		}
	}
}

// A host that accepts the connection and never answers is reported as
// unreachable instead of holding the fetch open.
func TestFetchPackageResourceTimesOutWaitingForHeaders(t *testing.T) {
	server := stallingServer(t, func(_ http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	client := newPackageFetchClient(shortPackageFetchTimeouts)
	_, err := fetchWithDeadline(t, func() ([]byte, error) {
		return fetchPackageResource(context.Background(), client, server.URL, "the plugin")
	})
	requirePackageError(t, err, "couldn't reach the host for the plugin")
	requireTransient(t, err, true)
}

// A host that sends headers and then stops sending the body is treated as an
// interrupted download.
func TestFetchPackageResourceTimesOutStalledBody(t *testing.T) {
	server := stallingServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	client := newPackageFetchClient(shortPackageFetchTimeouts)
	_, err := fetchWithDeadline(t, func() ([]byte, error) {
		return fetchPackageResourceWithIdle(context.Background(), client, server.URL, "the plugin", 100*time.Millisecond)
	})
	requirePackageError(t, err, "download of the plugin was interrupted")
	requireTransient(t, err, true)
	if !errors.Is(err, errPackageReadStalled) {
		t.Fatalf("stalled body error = %v, want errPackageReadStalled", err)
	}
}

// Only a wait for data counts against the idle timeout: a body that takes
// longer than the timeout in total but keeps sending still completes.
func TestFetchPackageResourceAllowsSlowProgressingBody(t *testing.T) {
	const (
		idle   = 500 * time.Millisecond
		chunks = 20
		gap    = 50 * time.Millisecond
	)
	server := stallingServer(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		w.WriteHeader(http.StatusOK)
		for range chunks {
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(gap)
		}
	})
	client := newPackageFetchClient(shortPackageFetchTimeouts)
	start := time.Now()
	data, err := fetchWithDeadline(t, func() ([]byte, error) {
		return fetchPackageResourceWithIdle(context.Background(), client, server.URL, "the plugin", idle)
	})
	if err != nil {
		t.Fatalf("slow download failed: %v", err)
	}
	if !bytes.Equal(data, bytes.Repeat([]byte("x"), chunks)) {
		t.Fatalf("slow download = %q", data)
	}
	if elapsed := time.Since(start); elapsed <= idle {
		t.Fatalf("download took %s, want longer than the %s idle timeout for the test to mean anything", elapsed, idle)
	}
}

// Canceling the caller's context during a stalled body still returns the
// caller's cancellation, not a stall.
func TestFetchPackageResourceCancelDuringStalledBody(t *testing.T) {
	bodyStarted := make(chan struct{})
	server := stallingServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		close(bodyStarted)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	client := newPackageFetchClient(shortPackageFetchTimeouts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-bodyStarted
		cancel()
	}()
	_, err := fetchWithDeadline(t, func() ([]byte, error) {
		return fetchPackageResourceWithIdle(ctx, client, server.URL, "the plugin", time.Hour)
	})
	if _, ok := AsPackageError(err); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fetch = %v, want context.Canceled and no PackageError", err)
	}
}
