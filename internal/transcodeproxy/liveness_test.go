package transcodeproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/nodepool"
)

// shortLiveness shortens the liveness timings for one test.
func shortLiveness(t *testing.T) livenessTiming {
	t.Helper()
	timing := livenessTiming{grace: 100 * time.Millisecond, probeTimeout: 200 * time.Millisecond, interval: 100 * time.Millisecond}
	liveness.Store(&timing)
	t.Cleanup(func() {
		liveness.Store(nil)
		notAnswering.Clear()
	})
	return timing
}

// frozenNode accepts connections and never answers anything, health included,
// as a stopped process does. It counts health probes.
func frozenNode(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var probes atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			probes.Add(1)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server, &probes
}

func nodeGet(t *testing.T, ctx context.Context, nodeURL string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nodeURL+"/transcode/t/segment/seg_00001.ts", nil)
	if err != nil {
		t.Fatal(err)
	}
	return DoNode(NodeClient(), req, nodeURL, "stream")
}

func TestDoNodeFailsCallToHungNode(t *testing.T) {
	timing := shortLiveness(t)
	node, _ := frozenNode(t)

	started := time.Now()
	resp, err := nodeGet(t, context.Background(), node.URL)
	took := time.Since(started)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, nodepool.ErrNodeNotAnswering) || !nodepool.IsConnectFailure(err) {
		t.Fatalf("err = %v, want ErrNodeNotAnswering counted as a connect failure", err)
	}
	if bound := timing.grace + timing.probeTimeout + time.Second; took > bound {
		t.Fatalf("hung node detected after %v, want within %v", took, bound)
	}
}

func TestDoNodeWaitsOnSlowLiveNode(t *testing.T) {
	timing := shortLiveness(t)
	var probes atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			probes.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable) // any answer means alive
			return
		}
		// Answer only after several probes have found the node alive.
		time.Sleep(timing.grace + 5*timing.interval)
		_, _ = io.WriteString(w, "segment")
	}))
	t.Cleanup(node.Close)

	resp, err := nodeGet(t, context.Background(), node.URL)
	if err != nil {
		t.Fatalf("slow live node failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "segment" {
		t.Fatalf("body = %q", body)
	}
	if got := probes.Load(); got < 2 {
		t.Fatalf("probes = %d, want the node probed while the call waited", got)
	}
}

func TestDoNodeLeavesCallerCancellationAlone(t *testing.T) {
	shortLiveness(t)
	node, _ := frozenNode(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	resp, err := nodeGet(t, ctx, node.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || errors.Is(err, nodepool.ErrNodeNotAnswering) || nodepool.IsConnectFailure(err) {
		t.Fatalf("err = %v, want the caller's own deadline, not a hung node", err)
	}
}

// TestDoNodeProbesKnownHungNodeAtOnce pins the memory: once a node is found
// hung, later calls skip the grace, and concurrent calls share one probe.
func TestDoNodeProbesKnownHungNodeAtOnce(t *testing.T) {
	timing := shortLiveness(t)
	timing.grace = 5 * time.Second
	liveness.Store(&timing)
	node, probes := frozenNode(t)
	notAnswering.Store(nodepool.NormalizeNodeURL(node.URL), struct{}{})

	const calls = 4
	errs := make(chan error, calls)
	started := time.Now()
	for range calls {
		go func() {
			resp, err := nodeGet(t, context.Background(), node.URL)
			if resp != nil {
				_ = resp.Body.Close()
			}
			errs <- err
		}()
	}
	for range calls {
		if err := <-errs; !errors.Is(err, nodepool.ErrNodeNotAnswering) {
			t.Fatalf("err = %v, want ErrNodeNotAnswering", err)
		}
	}
	if took := time.Since(started); took > timing.grace {
		t.Fatalf("known hung node took %v, want no grace wait", took)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("health probes = %d, want one shared by every call", got)
	}
}

func TestDoNodeForgetsNodeThatAnswersAgain(t *testing.T) {
	shortLiveness(t)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(node.Close)
	key := nodepool.NormalizeNodeURL(node.URL)
	notAnswering.Store(key, struct{}{})

	resp, err := nodeGet(t, context.Background(), node.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if _, known := notAnswering.Load(key); known {
		t.Fatal("a node that answered its probe is still remembered as hung")
	}
}
