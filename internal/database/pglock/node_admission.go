package pglock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// ErrAdmissionRejoining reports a node whose admission session failed and has
// not been replaced yet. Its storage writes are paused until it rejoins.
var ErrAdmissionRejoining = errors.New("storage node admission is rejoining")

// WriteGate pauses this process's storage writes and returns the function that
// resumes them. It must return when ctx ends.
type WriteGate func(ctx context.Context) (resume func(), err error)

// NodeAdmission owns one PostgreSQL session for a process's storage lifetime.
// The session must stay pinned: session advisory locks disappear if a pooled
// connection is returned or its PostgreSQL backend exits.
//
// A session that fails while this node holds only the shared lock is replaced.
// Rejoining succeeds only while no transition holds the exclusive lock, so a
// node never resumes writing beside a transition. Losing the session while this
// node owns a transition, or finding another owner when rejoining, is final.
type NodeAdmission struct {
	mu          sync.Mutex
	pool        *pgxpool.Pool
	conn        *pgx.Conn
	key         int64
	exclusive   bool
	closed      bool
	failed      bool
	lost        chan struct{}
	gate        WriteGate
	cancelPause context.CancelFunc
	resume      func()
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
	return &NodeAdmission{pool: pool, conn: conn.Hijack(), key: key, lost: make(chan struct{})}, nil
}

// SetWriteGate installs the pause used while a failed session is replaced.
// Main installs it once its blob stores are open; before then nothing writes.
func (a *NodeAdmission) SetWriteGate(gate WriteGate) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gate = gate
}

// TryExclusive upgrades this node's own shared lock without waiting for other
// API processes. PostgreSQL keeps the shared lock if another node prevents the
// upgrade, so normal storage service can continue after a rejected transition.
func (a *NodeAdmission) TryExclusive(ctx context.Context) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.failed {
		return false, ErrAdmissionLost
	}
	if a.conn == nil {
		return false, ErrAdmissionRejoining
	}
	if a.exclusive {
		return false, nil
	}
	var acquired bool
	if err := a.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, a.key).Scan(&acquired); err != nil {
		// The upgrade may have been granted before the error, so the session
		// cannot be treated as shared-only and replaced.
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
	if a.closed || a.failed {
		return ErrAdmissionLost
	}
	// An exclusive owner is never rejoining: losing its session is final.
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

// Probe checks the pinned session and replaces a failed shared-only session.
// It returns ErrAdmissionLost once admission is gone for good, and
// ErrAdmissionRejoining while the node is still waiting to rejoin.
func (a *NodeAdmission) Probe(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.failed {
		return ErrAdmissionLost
	}
	if a.conn != nil {
		err := a.conn.Ping(ctx)
		if err == nil {
			return nil
		}
		if a.exclusive {
			a.markLost()
			return fmt.Errorf("%w: probe failed while owning a transition: %w", ErrAdmissionLost, err)
		}
		slog.WarnContext(ctx, "storage node admission session failed; rejoining", "error", err)
		a.dropSessionLocked()
	}
	return a.rejoinLocked(ctx)
}

// rejoinLocked admits the node on a new session without waiting. A transition
// that took the exclusive lock while this node was out keeps it until its own
// process exits, so a refused rejoin is final. A database that cannot be
// reached leaves writes paused and the node rejoining.
func (a *NodeAdmission) rejoinLocked(ctx context.Context) error {
	conn, err := a.pool.Acquire(ctx)
	if err != nil {
		a.pauseWritesLocked()
		return fmt.Errorf("%w: %w", ErrAdmissionRejoining, err)
	}
	var joined bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock_shared($1)`, a.key).Scan(&joined); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = conn.Hijack().Close(closeCtx)
		cancel()
		a.pauseWritesLocked()
		return fmt.Errorf("%w: %w", ErrAdmissionRejoining, err)
	}
	if !joined {
		conn.Release()
		a.pauseWritesLocked()
		a.markLost()
		return fmt.Errorf("%w: another node owns a storage transition", ErrAdmissionLost)
	}
	a.conn = conn.Hijack()
	a.resumeWritesLocked()
	slog.InfoContext(ctx, "storage node admission rejoined")
	return nil
}

func (a *NodeAdmission) dropSessionLocked() {
	conn := a.conn
	a.conn = nil
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Close(closeCtx)
}

// pauseWritesLocked starts the write gate once. Waiting for in-flight writes
// happens off the lock; the fences queue new writers as soon as they wait.
func (a *NodeAdmission) pauseWritesLocked() {
	if a.gate == nil || a.cancelPause != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancelPause = cancel
	gate := a.gate
	go func() {
		resume, err := gate(ctx)
		if err != nil {
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if ctx.Err() != nil {
			// Rejoined or closed while the gate was waiting.
			resume()
			return
		}
		a.resume = resume
	}()
}

func (a *NodeAdmission) resumeWritesLocked() {
	if a.cancelPause != nil {
		a.cancelPause()
		a.cancelPause = nil
	}
	if a.resume != nil {
		a.resume()
		a.resume = nil
	}
}

// Monitor probes the pinned session until shutdown or final loss. A failed
// shared-only session is replaced on the next probes; Lost closes only when
// admission cannot be regained safely. Detection is bounded by interval plus
// the probe timeout, so storage transitions still require a maintenance window.
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
			if errors.Is(err, ErrAdmissionLost) {
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
	a.resumeWritesLocked()
	conn := a.conn
	a.conn = nil
	if conn == nil {
		return nil
	}
	return conn.Close(ctx)
}

// markLost is called with mu held. Paused writes stay paused: the process is
// about to stop, and nothing may write beside the transition that caused it.
// Close still destroys the connection even after a failed query with an
// uncertain lock outcome.
func (a *NodeAdmission) markLost() {
	if !a.failed {
		a.failed = true
		close(a.lost)
	}
}
