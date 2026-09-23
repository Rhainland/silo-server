package pglock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNodeAdmissionUpgradeAndJoin(t *testing.T) {
	pool := testPool(t)
	key := time.Now().UnixNano()
	owner, err := AdmitNode(t.Context(), pool, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	peer, err := AdmitNode(t.Context(), pool, key)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := owner.TryExclusive(t.Context())
	if err != nil || acquired {
		t.Fatalf("upgrade with a second shared holder = (%t, %v), want unavailable", acquired, err)
	}
	if err := peer.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Closing a client socket can return just before PostgreSQL has retired
	// that backend's session lock. Observe the upgrade rather than assuming
	// the server processed the close in the same scheduling turn.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		acquired, err = owner.TryExclusive(t.Context())
		if err != nil || acquired {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("peer session lock remained after close")
		}
	}
	if err != nil || !acquired {
		t.Fatalf("upgrade after peer exit = (%t, %v), want exclusive", acquired, err)
	}

	joinCtx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	joined, err := AdmitNode(joinCtx, pool, key)
	if joined != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("join during exclusive ownership = (%v, %v), want deadline", joined, err)
	}
	if err := owner.ReleaseExclusive(t.Context()); err != nil {
		t.Fatal(err)
	}
	joined, err = AdmitNode(t.Context(), pool, key)
	if err != nil {
		t.Fatalf("join after exclusive release: %v", err)
	}
	if err := joined.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestNodeAdmissionLossClosesSignalAndRejectsUpgrade(t *testing.T) {
	pool := testPool(t)
	owner, err := AdmitNode(t.Context(), pool, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	var pid int
	if err := owner.conn.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate admission backend = (%t, %v)", terminated, err)
	}
	if err := owner.Probe(t.Context()); err == nil {
		t.Fatal("probe accepted a lost admission session")
	}
	select {
	case <-owner.Lost():
	default:
		t.Fatal("lost admission signal was not closed")
	}
	if acquired, err := owner.TryExclusive(t.Context()); acquired || !errors.Is(err, ErrAdmissionLost) {
		t.Fatalf("upgrade after loss = (%t, %v), want ErrAdmissionLost", acquired, err)
	}
}

func TestNodeAdmissionProbeDeadlineSignalsLoss(t *testing.T) {
	pool := testPool(t)
	owner, err := AdmitNode(t.Context(), pool, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	probeCtx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if err := owner.Probe(probeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired probe = %v, want deadline", err)
	}
	select {
	case <-owner.Lost():
	default:
		t.Fatal("probe timeout left the lost admission signal open")
	}
}
