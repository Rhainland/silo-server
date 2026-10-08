package downloads

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalDownloadCanceledReadReturnsCommittedError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.mp4")
	payload := bytes.Repeat([]byte("synthetic media"), 8192)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	svc := &Service{bandwidth: NewBandwidthManager(131072, 0)}
	w := httptest.NewRecorder()
	err := svc.serveLocalFile(ctx, w, httptest.NewRequest("GET", "/file", nil), path, 2)
	if !errors.Is(err, ErrResponseCommitted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled body read must report a committed response error: %v", err)
	}
	if w.Body.Len() >= len(payload) {
		t.Fatalf("canceled transfer unexpectedly delivered the complete body: %d bytes", w.Body.Len())
	}
}
