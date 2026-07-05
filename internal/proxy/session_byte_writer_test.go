package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/nodesessions"
)

// rfResponseWriter is the sendfile-capable writer shape: an http.ResponseWriter
// that also implements io.ReaderFrom. It records whether ReadFrom was taken.
type rfResponseWriter struct {
	buf    bytes.Buffer
	hdr    http.Header
	usedRF bool
}

func (w *rfResponseWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *rfResponseWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }
func (w *rfResponseWriter) WriteHeader(int)             {}
func (w *rfResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	w.usedRF = true
	return w.buf.ReadFrom(src)
}

// plainResponseWriter implements ONLY http.ResponseWriter (no ReadFrom), forcing
// sessionByteWriter.ReadFrom down its manual-copy fallback.
type plainResponseWriter struct {
	buf bytes.Buffer
	hdr http.Header
}

func (w *plainResponseWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *plainResponseWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }
func (w *plainResponseWriter) WriteHeader(int)             {}

// TestSessionByteWriterReadFromFastPath verifies that when the underlying writer
// supports sendfile (io.ReaderFrom), ReadFrom forwards to it (preserving
// zero-copy) and still attributes the served bytes.
func TestSessionByteWriterReadFromFastPath(t *testing.T) {
	under := &rfResponseWriter{}
	sw := &sessionByteWriter{ResponseWriter: under, sessionID: "s"}

	const payload = "the quick brown fox jumps"
	n, err := sw.ReadFrom(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if !under.usedRF {
		t.Fatal("expected the underlying io.ReaderFrom (sendfile) fast path to be used")
	}
	if n != int64(len(payload)) {
		t.Fatalf("bytes forwarded = %d, want %d", n, len(payload))
	}
	if sw.acc != int64(len(payload)) {
		t.Fatalf("accounted bytes = %d, want %d", sw.acc, len(payload))
	}
	if got := under.buf.String(); got != payload {
		t.Fatalf("served body = %q, want %q", got, payload)
	}
}

// TestMeteredWriterChainPreservesSendfile proves the PRODUCTION writer chain —
// sessionByteWriter over meteredResponseWriter (every stream route runs inside
// meterEgress) — still reaches the underlying sendfile fast path AND meters the
// bytes. Regression guard: meteredResponseWriter used to hide io.ReaderFrom,
// which silently forced every proxied pour through a userspace copy no matter
// what the inner writer forwarded, making sessionByteWriter's fast path dead
// code on real requests.
func TestMeteredWriterChainPreservesSendfile(t *testing.T) {
	under := &rfResponseWriter{}
	meter := newEgressMeter()
	mw := &meteredResponseWriter{ResponseWriter: under, meter: meter}
	sw := &sessionByteWriter{ResponseWriter: mw, sessionID: "s"}

	const payload = "kernel to socket, no userspace detours"
	n, err := sw.ReadFrom(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if !under.usedRF {
		t.Fatal("expected sendfile fast path through the full metered chain")
	}
	if n != int64(len(payload)) {
		t.Fatalf("bytes forwarded = %d, want %d", n, len(payload))
	}
	if sw.acc != int64(len(payload)) {
		t.Fatalf("session-accounted bytes = %d, want %d", sw.acc, len(payload))
	}
	var metered int64
	meter.mu.Lock()
	for _, b := range meter.buckets {
		metered += b
	}
	meter.mu.Unlock()
	if metered != int64(len(payload)) {
		t.Fatalf("metered bytes = %d, want %d", metered, len(payload))
	}
	if got := under.buf.String(); got != payload {
		t.Fatalf("served body = %q, want %q", got, payload)
	}
}

// TestSessionByteWriterAccountFlushBranch exercises account()'s coarse-flush
// branch (w.acc >= 1<<20) that the short-payload tests never reach: a >=1MiB
// pour must flush the accumulator to the tracker and reset acc to 0. It also
// pins that the flush is safe against a real *nodesessions.Tracker — the other
// tests leave tracker nil, which only stays safe while the branch is untaken. A
// Redis-less tracker makes AddBytes a no-op, so the branch runs without Redis.
func TestSessionByteWriterAccountFlushBranch(t *testing.T) {
	tracker := nodesessions.NewTracker(nil, "http://node", "node", "proxy")
	under := &rfResponseWriter{}
	sw := &sessionByteWriter{ResponseWriter: under, tracker: tracker, sessionID: "s"}

	const oneMiB = 1 << 20
	payload := strings.Repeat("x", oneMiB)
	n, err := sw.ReadFrom(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if !under.usedRF {
		t.Fatal("expected the sendfile fast path even for a large payload")
	}
	if n != int64(oneMiB) {
		t.Fatalf("bytes forwarded = %d, want %d", n, oneMiB)
	}
	// The >=1MiB pour must have tripped the flush branch, resetting the accumulator.
	if sw.acc != 0 {
		t.Fatalf("accumulator = %d, want 0 (coarse-flush branch not taken)", sw.acc)
	}
	if got := under.buf.Len(); got != oneMiB {
		t.Fatalf("served bytes = %d, want %d", got, oneMiB)
	}
}

// TestSessionByteWriterReadFromFallbackNoRecursion verifies the fallback path
// (underlying writer has no sendfile support) copies via Write without
// re-entering ReadFrom. A naive io.Copy(sw, src) fallback would re-detect this
// ReadFrom and recurse until the stack overflows, so simply returning here — and
// counting the bytes — proves the guard works.
func TestSessionByteWriterReadFromFallbackNoRecursion(t *testing.T) {
	under := &plainResponseWriter{}
	sw := &sessionByteWriter{ResponseWriter: under, sessionID: "s"}

	const payload = "fallback path must not recurse"
	n, err := sw.ReadFrom(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("bytes copied = %d, want %d", n, len(payload))
	}
	if sw.acc != int64(len(payload)) {
		t.Fatalf("accounted bytes = %d, want %d", sw.acc, len(payload))
	}
	if got := under.buf.String(); got != payload {
		t.Fatalf("served body = %q, want %q", got, payload)
	}
}
