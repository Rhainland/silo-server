package pglock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StorageNodeAdmissionLockKey coordinates write-capable API processes with a
// storage transition. Each process holds the shared lock while it can write
// blobs; the transition owner takes the exclusive lock on the same session.
const StorageNodeAdmissionLockKey int64 = 0x53494c4f53544e

var ErrAdmissionLost = errors.New("storage node admission lock was lost")

// NodeAdmission owns one PostgreSQL session for a process's storage lifetime.
// The session must stay pinned: session advisory locks disappear if a pooled
// connection is returned or its PostgreSQL backend exits.
type NodeAdmission struct {
	mu        sync.Mutex
	conn      *pgx.Conn
	key       int64
	exclusive bool
	closed    bool
	failed    bool
	lost      chan struct{}
}

// AdmitNode waits for any transitioning owner, then admits this process before
// it opens storage or starts writers. A failed acquisition destroys the session
// because PostgreSQL may have granted the lock just before reporting an error.
func AdmitNode(ctx context.Context, pool *pgxpool.Pool, key int64) (*NodeAdmission, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire storage admission connection: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock_shared($1)`, key); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = conn.Hijack().Close(closeCtx)
		cancel()
		return nil, fmt.Errorf("acquire shared storage admission lock: %w", err)
	}
	// Detach from the pool so its shutdown cannot release the lock while old
	// storage workers are still draining. Main keeps it until process exit.
	return &NodeAdmission{conn: conn.Hijack(), key: key, lost: make(chan struct{})}, nil
}

// TryExclusive upgrades this node's own shared lock without waiting for other
// API processes. PostgreSQL keeps the shared lock if another node prevents the
// upgrade, so normal storage service can continue after a rejected transition.
func (a *NodeAdmission) TryExclusive(ctx context.Context) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.failed || a.conn == nil {
		return false, ErrAdmissionLost
	}
	if a.exclusive {
		return false, nil
	}
	var acquired bool
	if err := a.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, a.key).Scan(&acquired); err != nil {
		a.markLost()
		return false, fmt.Errorf("upgrade storage admission lock: %w", err)
	}
	a.exclusive = acquired
	return acquired, nil
}

// ReleaseExclusive leaves the process's shared lock held after an uncommitted
// transition fails or is canceled. A committed transition retains exclusivity
// until the old process has drained and exits.
func (a *NodeAdmission) ReleaseExclusive(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.failed || a.conn == nil {
		return ErrAdmissionLost
	}
	if !a.exclusive {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var released bool
	if err := a.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, a.key).Scan(&released); err != nil {
		a.markLost()
		return fmt.Errorf("release exclusive storage admission lock: %w", err)
	}
	if !released {
		a.markLost()
		return ErrAdmissionLost
	}
	a.exclusive = false
	return nil
}

// Probe checks the pinned session. A failed probe marks admission lost and
// prevents this process from admitting further storage transitions.
func (a *NodeAdmission) Probe(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.failed || a.conn == nil {
		return ErrAdmissionLost
	}
	if err := a.conn.Ping(ctx); err != nil {
		a.markLost()
		return fmt.Errorf("probe storage admission lock: %w", err)
	}
	return nil
}

// Monitor probes the pinned session until shutdown or lock loss. A failed
// probe closes Lost so the server can immediately begin draining. Detection is
// bounded by interval plus the probe timeout, not simultaneous with a network
// failure; storage transitions therefore still require a maintenance window.
func (a *NodeAdmission) Monitor(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.lost:
			return
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, 2*interval)
			err := a.Probe(probeCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (a *NodeAdmission) Lost() <-chan struct{} { return a.lost }

// Close destroys the pinned session after all HTTP handlers and workers have
// drained. Closing the backend releases both lock modes together.
func (a *NodeAdmission) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	conn := a.conn
	a.conn = nil
	if conn == nil {
		return nil
	}
	return conn.Close(ctx)
}

// markLost is called with mu held. Close still destroys the connection even
// after a failed query with an uncertain lock outcome.
func (a *NodeAdmission) markLost() {
	if !a.failed {
		a.failed = true
		close(a.lost)
	}
}
