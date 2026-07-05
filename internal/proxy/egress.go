package proxy

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// meterWindowSeconds is the averaging window for the egress rate. HLS clients
// fetch segments in bursts (especially when buffering ahead), so a window of
// this size smooths the spikes into something close to the steady-state
// stream rate. The planner's bandwidth reservation bridge matches this value.
const meterWindowSeconds = 60

// egressMeter measures outbound stream bytes as a rolling per-second ring,
// reporting the average rate over the window. Safe for concurrent use.
type egressMeter struct {
	mu sync.Mutex
	// One extra bucket so the current (partial) second never collides with
	// the oldest second still inside the window.
	buckets [meterWindowSeconds + 1]int64
	stamps  [meterWindowSeconds + 1]int64 // unix second each bucket holds
	now     func() time.Time
}

func newEgressMeter() *egressMeter {
	return &egressMeter{now: time.Now}
}

// Add records n egressed bytes against the current second.
func (m *egressMeter) Add(n int64) {
	if n <= 0 {
		return
	}
	sec := m.now().Unix()
	i := int(sec % int64(len(m.buckets)))
	m.mu.Lock()
	if m.stamps[i] != sec {
		m.stamps[i] = sec
		m.buckets[i] = 0
	}
	m.buckets[i] += n
	m.mu.Unlock()
}

// RateKbps returns the average egress over the window in kilobits/s.
func (m *egressMeter) RateKbps() int {
	sec := m.now().Unix()
	var total int64
	m.mu.Lock()
	for i := range m.buckets {
		if sec-m.stamps[i] < meterWindowSeconds {
			total += m.buckets[i]
		}
	}
	m.mu.Unlock()
	return int(total * 8 / 1000 / meterWindowSeconds)
}

// meteredResponseWriter counts every byte written to the client. Embedding the
// interface hides optimizations like io.ReaderFrom, so ReadFrom is forwarded
// explicitly below — every stream handler runs inside meterEgress, so without
// that forwarding NO proxied pour could ever reach the kernel sendfile path,
// regardless of what the inner writers (sessionByteWriter) forward.
type meteredResponseWriter struct {
	http.ResponseWriter
	meter *egressMeter
}

func (w *meteredResponseWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.meter.Add(int64(n))
	return n, err
}

// ReadFrom preserves the underlying ResponseWriter's sendfile fast path
// (disk→socket inside the kernel) while still metering the bytes: the count
// only needs the total, which sendfile reports on return. Without a ReaderFrom
// underneath it falls back to a plain copy through Write (which meters), via
// writeOnly so io.Copy cannot re-enter this method and recurse.
func (w *meteredResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		w.meter.Add(n)
		return n, err
	}
	return io.Copy(writeOnly{w}, src)
}

func (w *meteredResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.NewResponseController reach the underlying ResponseWriter so
// SetWriteDeadline (used by the revocation connection-cut) can find the socket.
func (w *meteredResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// meterEgress wraps stream handlers so their responses count toward the
// node's measured egress bandwidth.
func (s *Server) meterEgress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&meteredResponseWriter{ResponseWriter: w, meter: s.egress}, r)
	})
}
