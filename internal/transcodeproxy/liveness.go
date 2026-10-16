package transcodeproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/telemetry"
)

// A node that hangs (its process stopped or deadlocked while its kernel still
// accepts connections and acknowledges requests) never refuses a call, never
// fails one at the TCP layer, and never answers. The connect bound and
// TCP_USER_TIMEOUT in nodepool catch a node that is gone; neither catches this.
// A response deadline cannot either: a live node may hold a manifest or
// segment request while it rebuilds a transcode, or a start until FFmpeg is
// ready, for over a minute (see nodeClient).
//
// DoNode tells the two apart by asking the node. Once a call has waited
// livenessGrace for response headers, the node's health endpoint is probed,
// and probed again every livenessInterval while the call keeps waiting. A live
// node answers health at once however slow the call it is serving, and the call
// keeps waiting. A node that does not answer a probe within
// livenessProbeTimeout is hung: the call is canceled and fails with
// nodepool.ErrNodeNotAnswering.
//
// A node found hung is remembered until it answers a probe or a call again, and
// calls to it probe at once instead of waiting out the grace first, so every
// other session on the node finds out within one probe timeout.
const (
	livenessGrace        = 3 * time.Second
	livenessProbeTimeout = 2 * time.Second
	livenessInterval     = 3 * time.Second
)

type livenessTiming struct {
	grace, probeTimeout, interval time.Duration
}

var liveness atomic.Pointer[livenessTiming]

func currentLiveness() livenessTiming {
	if timing := liveness.Load(); timing != nil {
		return *timing
	}
	return livenessTiming{grace: livenessGrace, probeTimeout: livenessProbeTimeout, interval: livenessInterval}
}

// DoNode sends req to the transcode node at nodeURL through
// telemetry.DoTrustedNode and watches the node while the call waits for
// response headers. It returns an error wrapping nodepool.ErrNodeNotAnswering
// when the node stopped answering, which nodepool.IsConnectFailure counts as a
// failure to reach the node. The watch ends with the headers: a body that stops
// streaming is not detected here.
func DoNode(client *http.Client, req *http.Request, nodeURL, operation string) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	const (
		waiting int32 = iota
		answered
		hung
	)
	var state atomic.Int32
	done := make(chan struct{})
	go watchNode(ctx, nodeURL, currentLiveness(), done, func() {
		if state.CompareAndSwap(waiting, hung) {
			cancel(nodepool.ErrNodeNotAnswering)
		}
	})
	resp, err := telemetry.DoTrustedNode(client, req.WithContext(ctx), operation)
	close(done)
	if !state.CompareAndSwap(waiting, answered) {
		// The watch found the node hung before an answer arrived.
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("%w: no answer to %s or to a health probe", nodepool.ErrNodeNotAnswering, operation)
	}
	if err != nil {
		cancel(nil)
		return nil, err
	}
	// An answer is as good as a probe's.
	notAnswering.Delete(nodepool.NormalizeNodeURL(nodeURL))
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: func() { cancel(nil) }}
	return resp, nil
}

// cancelOnClose releases the call's context once its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel func()
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// watchNode probes nodeURL while a call to it waits, and calls hung once a
// probe goes unanswered. It returns when the call is answered or given up.
func watchNode(ctx context.Context, nodeURL string, timing livenessTiming, done <-chan struct{}, hung func()) {
	wait := timing.grace
	if _, known := notAnswering.Load(nodepool.NormalizeNodeURL(nodeURL)); known {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case ok := <-probeNode(nodeURL, timing.probeTimeout):
			if !ok {
				hung()
				return
			}
		}
		timer.Reset(timing.interval)
	}
}

var (
	// probes shares one health probe among every call waiting on a node.
	probes singleflight.Group
	// notAnswering holds the nodes whose last probe went unanswered.
	notAnswering sync.Map
)

// probeNode reports, on the returned channel, whether the node answered a
// health request within timeout. The probe runs detached from any one caller,
// so a caller that gives up cannot fail it for the others sharing it.
func probeNode(nodeURL string, timeout time.Duration) <-chan bool {
	key := nodepool.NormalizeNodeURL(nodeURL)
	result := make(chan bool, 1)
	shared := probes.DoChan(key, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		ok := nodeAnswers(ctx, key)
		if ok {
			notAnswering.Delete(key)
		} else {
			notAnswering.Store(key, struct{}{})
		}
		return ok, nil
	})
	go func() {
		res := <-shared
		ok, _ := res.Val.(bool)
		result <- ok
	}()
	return result
}

// probeClient keeps health probes off the relay connection pool, whose
// connections to a hung node are all stuck.
var probeClient = &http.Client{
	Transport: newProbeTransport(),
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func newProbeTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	t := base.Clone()
	t.DialContext = nodepool.NodeDialContext()
	t.MaxIdleConnsPerHost = 2
	return t
}

// nodeAnswers reports whether the node answers its unauthenticated health
// endpoint at all. Any HTTP response counts: the question is whether the
// process is serving requests, not whether it calls itself healthy.
func nodeAnswers(ctx context.Context, nodeURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nodepool.NodeEndpoint(nodeURL, "/api/v1/health"), nil)
	if err != nil {
		return false
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return true
}
