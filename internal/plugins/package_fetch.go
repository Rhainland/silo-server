package plugins

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// Plugin catalogs, checksum files and packages are fetched without a
// whole-request deadline, because a package can be up to 256 MiB and a slow
// but healthy download must still finish. Each phase that can hang is bounded
// instead: connecting, the TLS handshake, waiting for response headers, and
// any single wait for more of the body.
const (
	packageFetchDialTimeout           = 10 * time.Second
	packageFetchTLSHandshakeTimeout   = 10 * time.Second
	packageFetchResponseHeaderTimeout = 30 * time.Second
	packageFetchReadIdleTimeout       = 60 * time.Second
)

// errPackageReadStalled is the cause recorded when a body sends nothing for
// longer than the read-idle timeout.
var errPackageReadStalled = errors.New("download stalled: no data received within the idle timeout")

// packageFetchTimeouts bounds the transport phases of a package fetch.
type packageFetchTimeouts struct {
	dial           time.Duration
	tlsHandshake   time.Duration
	responseHeader time.Duration
}

var defaultPackageFetchTimeouts = packageFetchTimeouts{
	dial:           packageFetchDialTimeout,
	tlsHandshake:   packageFetchTLSHandshakeTimeout,
	responseHeader: packageFetchResponseHeaderTimeout,
}

// newPackageFetchClient returns the client used when the catalog service or
// installer is given none. It keeps the default transport's proxy and
// connection-pool settings and redirect handling.
func newPackageFetchClient(timeouts packageFetchTimeouts) *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	transport := base.Clone()
	transport.DialContext = (&net.Dialer{Timeout: timeouts.dial, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = timeouts.tlsHandshake
	transport.ResponseHeaderTimeout = timeouts.responseHeader
	return &http.Client{Transport: transport}
}

// idleTimeoutReader cancels the fetch when one Read waits longer than idle.
// Only time blocked in Read counts, so a slow body that keeps sending data is
// never cut off.
type idleTimeoutReader struct {
	r     io.Reader
	idle  time.Duration
	timer *time.Timer
}

func newIdleTimeoutReader(r io.Reader, idle time.Duration, cancel context.CancelCauseFunc) *idleTimeoutReader {
	timer := time.AfterFunc(idle, func() { cancel(errPackageReadStalled) })
	timer.Stop()
	return &idleTimeoutReader{r: r, idle: idle, timer: timer}
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	r.timer.Reset(r.idle)
	n, err := r.r.Read(p)
	r.timer.Stop()
	return n, err
}

func (r *idleTimeoutReader) stop() { r.timer.Stop() }
