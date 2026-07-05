package streamrevoke

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// permanentExpiry is the far-future sentinel written when a Revocation has a
// zero-value ExpiresAt. The hot path treats a zero ExpiresAt as "never expires"
// (a permanent kill), but the DB column is NOT NULL and Prune/ListActive compare
// expires_at <= now(): a literal zero time (0001-01-01) would be excluded by
// ListActive and deleted by the very next Prune, silently evaporating a
// permanent kill. Writing a year-2999 sentinel preserves the intent durably.
var permanentExpiry = time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)

// PostgresDurableStore is the Postgres-backed DurableStore: a durable mirror of
// the kill list so revocations survive a Redis flush or a server restart. It is
// never on the hot path — Store consults it only on write (Upsert), on
// warm/reconcile (ListActive), and on trim (Prune).
//
// Rows are keyed by (kind, id) so re-revoking the same session/user (the async
// over-cap enforcer does this every pass) UPSERTs the same row rather than
// accumulating duplicates; physical growth is reclaimed by Prune.
type PostgresDurableStore struct {
	pool *pgxpool.Pool
}

// NewPostgresDurableStore builds a DurableStore from a pgx pool. It returns a
// nil DurableStore interface when pool is nil so callers can pass the result
// straight into Options.Durable and a Redis-less/DB-less mode degrades to a
// true nil interface (avoiding the "non-nil interface wrapping a nil pointer"
// trap that would make Store.durable != nil erroneously true).
func NewPostgresDurableStore(pool *pgxpool.Pool) DurableStore {
	if pool == nil {
		return nil
	}
	return &PostgresDurableStore{pool: pool}
}

// Upsert writes or refreshes a revocation, keyed by (kind, id).
func (s *PostgresDurableStore) Upsert(ctx context.Context, r Revocation) error {
	// A zero ExpiresAt means "permanent" on the hot path; persist it as a
	// far-future sentinel so Prune/ListActive don't immediately reap the row.
	expiresAt := r.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = permanentExpiry
	}
	// Expiry is monotonic: a re-revoke never shortens an existing longer kill
	// (GREATEST). The async over-cap enforcer re-revokes with a short 5m TTL, and
	// without this it would shrink an admin's 24h kill on the same session key and
	// reopen the restart-resurrection window. reason/revoked_at follow whichever
	// expiry wins so the persisted row stays coherent. Mirrors applyLocal.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO stream_revocations (kind, id, reason, revoked_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (kind, id) DO UPDATE SET
			reason = CASE WHEN EXCLUDED.expires_at >= stream_revocations.expires_at
				THEN EXCLUDED.reason ELSE stream_revocations.reason END,
			revoked_at = CASE WHEN EXCLUDED.expires_at >= stream_revocations.expires_at
				THEN EXCLUDED.revoked_at ELSE stream_revocations.revoked_at END,
			expires_at = GREATEST(stream_revocations.expires_at, EXCLUDED.expires_at)`,
		string(r.Kind), r.ID, r.Reason, r.RevokedAt, expiresAt)
	if err != nil {
		return fmt.Errorf("streamrevoke upsert: %w", err)
	}
	return nil
}

// ListActive returns every revocation not yet expired, for warming the hot-path
// cache on startup and re-warming after a Redis flush.
func (s *PostgresDurableStore) ListActive(ctx context.Context) ([]Revocation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind, id, reason, revoked_at, expires_at
		FROM stream_revocations
		WHERE expires_at > now()`)
	if err != nil {
		return nil, fmt.Errorf("streamrevoke list active: %w", err)
	}
	defer rows.Close()

	var out []Revocation
	for rows.Next() {
		var r Revocation
		var kind string
		if err := rows.Scan(&kind, &r.ID, &r.Reason, &r.RevokedAt, &r.ExpiresAt); err != nil {
			return nil, fmt.Errorf("streamrevoke scan: %w", err)
		}
		r.Kind = Kind(kind)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("streamrevoke list active rows: %w", err)
	}
	return out, nil
}

// Prune physically deletes expired rows so the table does not grow unbounded as
// the async enforcer re-revokes across passes.
func (s *PostgresDurableStore) Prune(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM stream_revocations WHERE expires_at <= now()`); err != nil {
		return fmt.Errorf("streamrevoke prune: %w", err)
	}
	return nil
}
