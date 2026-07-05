// Package streamenforcer is the async decision loop ("the brain") of the
// monitor-and-kill design. It runs on central only, off the hot path: it reads
// the authoritative live-streams snapshot, compares each user's live count to
// that user's limit, and issues revocations for the over-cap sessions. The
// revocation kill switch (internal/streamrevoke) then stops them at the edge
// within one propagation/poll interval.
//
// Every enforcement reason (over-cap here, admin terminate and account
// revocation elsewhere) collapses to the same action: write a revocation. This
// package owns only the over-cap rule; other reasons call the revoker directly.
package streamenforcer

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/Silo-Server/silo-server/internal/streammonitor"
)

// DefaultInterval is how often the enforcer evaluates the live picture. The
// ~120s enforcement budget = this interval + the revocation propagation/poll.
const DefaultInterval = 30 * time.Second

// revocationTTL is how long an over-cap revocation lasts. It is deliberately
// short: the enforcer re-evaluates every DefaultInterval, so a persistent abuser
// is re-revoked long before this lapses (staying dead), while a transient
// over-count — e.g. a ghost session lingering in the monitor next to a fresh
// reconnect — self-heals within this window instead of banning the reconnect for
// 24h. Must comfortably exceed DefaultInterval so re-revocation has slack.
const revocationTTL = 5 * time.Minute

// Revoker is the subset of *streamrevoke.Store the enforcer needs. It uses the
// TTL-scoped variant so an over-cap kill self-heals (see revocationTTL).
type Revoker interface {
	RevokeSessionFor(ctx context.Context, sessionID, reason string, ttl time.Duration) error
}

// LimitFunc returns the maximum concurrent streams allowed for a user. A return
// of <= 0 means "unlimited" (no enforcement for that user), matching the
// SessionManager's convention. An error means the limit is currently unknown;
// the enforcer fails OPEN (does not kill) so a limit-lookup blip never
// terminates legitimate playback.
type LimitFunc func(ctx context.Context, userID int) (maxStreams int, err error)

// Enforcer periodically trims over-cap streams.
type Enforcer struct {
	source   streammonitor.Source
	limits   LimitFunc
	revoker  Revoker
	interval time.Duration
	now      func() time.Time
}

// New builds an enforcer. interval <= 0 uses DefaultInterval.
func New(source streammonitor.Source, limits LimitFunc, revoker Revoker, interval time.Duration) *Enforcer {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Enforcer{
		source:   source,
		limits:   limits,
		revoker:  revoker,
		interval: interval,
		now:      time.Now,
	}
}

// Start runs the evaluation loop until ctx is cancelled. Non-blocking.
func (e *Enforcer) Start(ctx context.Context) {
	if e == nil || e.source == nil || e.limits == nil || e.revoker == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(e.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.evaluate(ctx)
			}
		}
	}()
}

// evaluate runs one pass: snapshot → per-user over-cap check → revoke victims.
// Exported behavior is covered by EvaluateOnce for tests.
func (e *Enforcer) evaluate(ctx context.Context) {
	if err := e.EvaluateOnce(ctx); err != nil {
		slog.Debug("stream enforcer evaluate failed", "error", err)
	}
}

// EvaluateOnce performs a single enforcement pass and returns the number of
// sessions revoked. Deterministic and side-effect-scoped for testing.
func (e *Enforcer) EvaluateOnce(ctx context.Context) error {
	snap, err := e.source.Snapshot(ctx)
	if err != nil {
		return err
	}
	for userID, streams := range snap.ByUser() {
		if userID <= 0 || len(streams) == 0 {
			// user 0 == records with no resolved owner; never enforce against it.
			continue
		}
		limit, err := e.limits(ctx, userID)
		if err != nil {
			// Fail open: a limit-lookup error must never kill legitimate streams.
			slog.Debug("stream enforcer: limit lookup failed; skipping user",
				"user_id", userID, "error", err)
			continue
		}
		if limit <= 0 || len(streams) <= limit {
			continue
		}
		for _, victim := range e.selectVictims(streams, limit) {
			if err := e.revoker.RevokeSessionFor(ctx, victim.SessionID, "over_concurrent_stream_limit", revocationTTL); err != nil {
				slog.Warn("stream enforcer: revoke failed",
					"user_id", userID, "session_id", victim.SessionID, "error", err)
				continue
			}
			slog.Info("stream enforcer: revoked over-cap session",
				"user_id", userID, "session_id", victim.SessionID,
				"limit", limit, "live", len(streams))
		}
	}
	return nil
}

// selectVictims returns the streams beyond the limit, keeping the `limit`
// MOST-RECENTLY-SERVED sessions and trimming the rest. Ordering by real serve
// activity (LastServedAt), not StartedAt, matters: after a network blip a client
// reconnects with a new session while the old ghost lingers in the monitor for
// up to its TTL. Keeping the freshly-served sessions means the live reconnect
// survives and the stale ghost is the one trimmed (and it would have aged out
// anyway). Falls back to StartedAt, then session id, for deterministic ties.
func (e *Enforcer) selectVictims(streams []streammonitor.LiveStream, limit int) []streammonitor.LiveStream {
	ordered := make([]streammonitor.LiveStream, len(streams))
	copy(ordered, streams)
	// Most-recently-served first.
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].LastServedAt.Equal(ordered[j].LastServedAt) {
			return ordered[i].LastServedAt.After(ordered[j].LastServedAt)
		}
		if !ordered[i].StartedAt.Equal(ordered[j].StartedAt) {
			return ordered[i].StartedAt.After(ordered[j].StartedAt)
		}
		return ordered[i].SessionID < ordered[j].SessionID
	})
	if limit >= len(ordered) {
		return nil
	}
	// Keep the first `limit` (freshest); revoke the rest (stalest).
	return ordered[limit:]
}
