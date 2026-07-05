// Package streamrevoke implements a stream-revocation "kill switch": a small
// set of revoked stream sessions/users that edge nodes consult on the hot path
// via an in-memory cache, backed by Redis for multi-node propagation with an
// optional durable Postgres mirror.
//
// The hot path (IsRevoked) is a pure in-memory read with no I/O. Revocations
// are propagated to other nodes over Redis pub/sub for immediate application
// and mirrored to Redis keys (with a TTL) so late-joining or restarting nodes
// can reconcile via a periodic SCAN. A durable mirror, when configured, lets
// kills survive a Redis flush.
package streamrevoke

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Silo-Server/silo-server/internal/cache"
	"github.com/redis/go-redis/v9"
)

const (
	// keyPrefix is the Redis key namespace for revocation mirror keys, e.g.
	// silo:revoked:sess:{id} and silo:revoked:user:{id}.
	keyPrefix = "silo:revoked:"

	// scanPattern matches every revocation mirror key for cache warming and
	// periodic reconciliation.
	scanPattern = keyPrefix + "*"

	// EventStreamRevoked is the cache.Event.Type published on cache.ChannelAdmin
	// when a revocation is created so other nodes apply it immediately.
	EventStreamRevoked = "stream_revoked"

	defaultPollInterval = 60 * time.Second
	// defaultTTL is intentionally >= the playback recipe-card lifetime
	// (playback.MaxTokenTTL, 24h): a kill must never expire before the session it
	// kills can be reconstructed, or PR #174's restart-resilient playback could
	// rebuild and re-serve a stream whose revocation had already lapsed. This is
	// an invariant, stated in words to avoid importing the playback package here
	// (which would create an import cycle); keep the two values coupled.
	defaultTTL = 24 * time.Hour
)

// Kind identifies what a revocation targets.
type Kind string

const (
	// KindSession revokes a single stream session id.
	KindSession Kind = "sess"
	// KindUser revokes every stream belonging to a user id.
	KindUser Kind = "user"
)

// Key uniquely identifies a revocation in the in-memory cache and Redis.
type Key struct {
	Kind Kind
	ID   string
}

// Revocation is a single kill-switch entry.
type Revocation struct {
	Kind      Kind      `json:"kind"`
	ID        string    `json:"id"`
	Reason    string    `json:"reason,omitempty"`
	RevokedAt time.Time `json:"revoked_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// key returns the in-memory cache key for this revocation.
func (r Revocation) key() Key {
	return Key{Kind: r.Kind, ID: r.ID}
}

// expired reports whether the revocation is no longer in force at now.
func (r Revocation) expired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt)
}

// DurableStore is an optional Postgres-backed mirror so kills survive a Redis
// flush. A concrete implementation lives elsewhere; this package only consumes
// the interface when one is provided.
type DurableStore interface {
	Upsert(ctx context.Context, r Revocation) error
	ListActive(ctx context.Context) ([]Revocation, error)
	Prune(ctx context.Context) error
}

// Options configures a Store.
type Options struct {
	Redis        *redis.Client  // nil => memory-only (integrated single-node)
	Bus          cache.EventBus // nil => no push propagation
	Durable      DurableStore   // nil => no durable mirror
	PollInterval time.Duration  // default 60s
	DefaultTTL   time.Duration  // default 24h
}

// Store holds the in-memory revocation cache and its propagation plumbing.
type Store struct {
	rdb          *redis.Client
	bus          cache.EventBus
	durable      DurableStore
	pollInterval time.Duration
	defaultTTL   time.Duration

	mu    sync.RWMutex
	items map[Key]Revocation
}

// New builds a Store, applying defaults for any unset Options.
func New(opts Options) *Store {
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.DefaultTTL <= 0 {
		opts.DefaultTTL = defaultTTL
	}
	return &Store{
		rdb:          opts.Redis,
		bus:          opts.Bus,
		durable:      opts.Durable,
		pollInterval: opts.PollInterval,
		defaultTTL:   opts.DefaultTTL,
		items:        make(map[Key]Revocation),
	}
}

// redisKey returns the Redis mirror key for a revocation key.
func redisKey(k Key) string {
	return keyPrefix + string(k.Kind) + ":" + k.ID
}

// userKey returns the cache key for a numeric user id.
func userKey(userID int) Key {
	return Key{Kind: KindUser, ID: strconv.Itoa(userID)}
}

// Refuse is the single shared enforcement point for every serve surface (edge
// proxy, transcode node, native api, jellycompat): if the session or user is
// revoked it writes a 403 and returns true, and the caller must stop serving.
// Centralizing the check + response here keeps one section per concern instead
// of duplicating "IsRevoked → 403" at each surface. It does NOT hang up an
// in-flight connection — long-pour paths pair this with a connection-cut helper.
// startedAt is when the request's stream credential was issued (see IsRevoked).
func (s *Store) Refuse(w http.ResponseWriter, sessionID string, userID int, startedAt time.Time) bool {
	if s == nil || !s.IsRevoked(sessionID, userID, startedAt) {
		return false
	}
	http.Error(w, "stream revoked", http.StatusForbidden)
	return true
}

// WatchAndCut watches a long-lived pour (a single long-GET direct-play/remux) and
// once the session/user is revoked, forces the in-flight write to fail via
// SetWriteDeadline — hanging up the socket even though the 24h token is still
// valid (cutting an open connection is a socket action, not a token revocation).
// It checks immediately on entry (so a pour that began the instant before the
// kill is cut without waiting a tick) and then every 5s. Returns a stop func the
// caller defers when the request finishes normally. This is the shared in-flight
// cut used by every long-pour serve surface (edge proxy, native api,
// jellycompat), so the cut logic lives in one place. HLS/transcode paths don't
// need it — per-segment Refuse stops them within one segment.
//
// Best-effort: if the ResponseWriter chain doesn't support write deadlines the
// deadline set is a no-op and the stream still stops on its next request via
// Refuse. Never wraps the writer, so it does not disable sendfile.
// startedAt follows IsRevoked's contract; a pour in flight when a user kill
// lands always predates that kill, so passing the request's credential/entry
// time makes mid-pour user kills cut correctly on every surface.
func (s *Store) WatchAndCut(w http.ResponseWriter, sessionID string, userID int, startedAt time.Time) func() {
	if s == nil {
		return func() {}
	}
	cut := func() { _ = http.NewResponseController(w).SetWriteDeadline(time.Now()) }
	if s.IsRevoked(sessionID, userID, startedAt) {
		cut()
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if s.IsRevoked(sessionID, userID, startedAt) {
					cut()
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// IsRevoked is the HOT PATH: a pure in-memory cache read with no I/O. It
// returns true if the session id is currently revoked, or if the user id is
// revoked AND the stream's credential predates that user revocation.
//
// startedAt is when the request's stream credential was issued: the stream
// token's iat on token-bearing surfaces (edge proxy, transcode node, native
// ?st=), or the request entry time on freshly-authenticated surfaces (native
// session auth, jellycompat login). A user revocation is a CUTOFF, not a ban:
// it kills streams whose credential predates it, while a stream authorized
// after it (which required passing auth that the revocation just reset) plays
// normally. Without the cutoff, the OnUserSessionsRevoked hook — fired by any
// admin edit of password/role/enabled/permissions/quality — would 403 the
// user's playback for the full 24h TTL even after they re-authenticate, with
// no unrevoke path (expiry is deliberately monotonic). A zero startedAt never
// matches a user revocation (fail open, matching the enforcer's "never kill on
// uncertainty"); session revocations are exact-id kills and ignore startedAt.
func (s *Store) IsRevoked(sessionID string, userID int, startedAt time.Time) bool {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sessionID != "" {
		if r, ok := s.items[Key{Kind: KindSession, ID: sessionID}]; ok && !r.expired(now) {
			return true
		}
	}
	// userID <= 0 is the "no resolved owner" sentinel (e.g. the transcode node's
	// session-only check). Never match it against a KindUser entry: a stray
	// user:"0" revocation must not read as "every ownerless request is revoked".
	if userID > 0 {
		if r, ok := s.items[userKey(userID)]; ok && !r.expired(now) &&
			!startedAt.IsZero() && startedAt.Before(r.RevokedAt) {
			return true
		}
	}
	return false
}

// effectiveExpiry returns the instant a revocation lapses, treating a zero
// ExpiresAt as "never" (far future) so monotonic comparisons order a permanent
// kill above any bounded one.
func (r Revocation) effectiveExpiry() time.Time {
	if r.ExpiresAt.IsZero() {
		return permanentExpiry
	}
	return r.ExpiresAt
}

// applyLocal inserts or refreshes a revocation in the in-memory cache, keeping
// whichever copy expires LATER. Expiry is monotonic: a re-revoke with a shorter
// TTL must never shorten a longer-lived kill. This matters because the async
// over-cap enforcer re-revokes with a short self-healing TTL (5m); without this
// guard it would silently shrink an admin's 24h RevokeSession on the same
// session key and reopen the restart-resurrection window PR #174 exposes. The
// same rule lets a mid-window Redis reconcile that reads a stale longer entry
// not fight a freshly-applied one. The caller must not hold s.mu.
func (s *Store) applyLocal(r Revocation) {
	s.mu.Lock()
	if existing, ok := s.items[r.key()]; !ok || !existing.effectiveExpiry().After(r.effectiveExpiry()) {
		s.items[r.key()] = r
	}
	s.mu.Unlock()
}

// effective returns the currently-stored revocation for a key — the
// monotonically-merged copy applyLocal kept (whichever expires later). Zero
// value if absent.
func (s *Store) effective(k Key) Revocation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.items[k]
}

// Revoke adds a revocation to the local cache, writes it to Redis (with a TTL
// of until-now) when configured, mirrors it to the durable store when
// configured, and publishes a pub/sub event so other nodes apply it
// immediately.
func (s *Store) Revoke(ctx context.Context, key Key, reason string, until time.Time) error {
	now := time.Now()
	r := Revocation{
		Kind:      key.Kind,
		ID:        key.ID,
		Reason:    reason,
		RevokedAt: now,
		ExpiresAt: until,
	}

	// Local cache first: the hot path must reflect the kill immediately even if
	// downstream propagation fails.
	s.applyLocal(r)

	// Propagation must not die with the caller: an admin terminate rides its
	// HTTP request's context, and an abort after applyLocal would strand the
	// kill in this process's memory — never reaching Redis (edges), pub/sub, or
	// the durable mirror (lost on restart).
	ctx = context.WithoutCancel(ctx)

	// Durable mirror (best-effort; failures are non-fatal).
	if s.durable != nil {
		if err := s.durable.Upsert(ctx, r); err != nil {
			slog.Warn("streamrevoke durable upsert failed", "error", err, "kind", r.Kind, "id", r.ID)
		}
	}

	// Redis mirror for multi-node propagation to edges. Mirror the merged copy
	// applyLocal kept (monotonic): a short over-cap re-revoke must not shorten a
	// longer admin kill's Redis TTL, matching applyLocal and the durable upsert's
	// GREATEST. Edges that warm from Redis in that window must see the later kill.
	s.mirrorToRedis(ctx, s.effective(r.key()))

	// Pub/sub push so already-connected nodes apply the kill immediately (the
	// warm loops deliberately skip this — SCAN reconcile handles late-joiners).
	if s.bus != nil {
		data, err := json.Marshal(r)
		if err != nil {
			slog.Warn("streamrevoke marshal failed", "error", err, "kind", r.Kind, "id", r.ID)
			return err
		}
		evt := cache.Event{Type: EventStreamRevoked, Payload: string(data)}
		if err := s.bus.Publish(ctx, cache.ChannelAdmin, evt); err != nil {
			slog.Warn("streamrevoke publish failed", "error", err, "kind", r.Kind, "id", r.ID)
		}
	}

	return nil
}

// mirrorToRedis writes (or deletes) the Redis mirror key for a revocation with a
// TTL of the time remaining until r.ExpiresAt. It is the single shared
// Redis-arming path, used both by Revoke and by the durable warm loops: edge
// nodes (proxy/transcode) have no durable store and learn kills ONLY from
// Redis, so re-arming Redis from the durable source of truth after a Redis flush
// + central restart is what lets edges reconverge via their SCAN reconcile.
// Best-effort: failures are logged, never returned. No-op when Redis is absent.
func (s *Store) mirrorToRedis(ctx context.Context, r Revocation) {
	if s.rdb == nil {
		return
	}
	data, err := json.Marshal(r)
	if err != nil {
		slog.Warn("streamrevoke marshal failed", "error", err, "kind", r.Kind, "id", r.ID)
		return
	}
	if r.ExpiresAt.IsZero() {
		// Permanent kill: set without a TTL, matching applyLocal/effectiveExpiry
		// treating a zero ExpiresAt as "never". A TTL of time.Until(zero) would be
		// hugely negative and drop the key, so late-joining edges would miss it.
		if err := s.rdb.Set(ctx, redisKey(r.key()), data, 0).Err(); err != nil {
			slog.Warn("streamrevoke redis set failed", "error", err, "kind", r.Kind, "id", r.ID)
		}
		return
	}
	ttl := time.Until(r.ExpiresAt)
	if ttl <= 0 {
		// Already expired: nothing to mirror in Redis.
		if err := s.rdb.Del(ctx, redisKey(r.key())).Err(); err != nil {
			slog.Debug("streamrevoke redis del failed", "error", err, "kind", r.Kind, "id", r.ID)
		}
		return
	}
	if err := s.rdb.Set(ctx, redisKey(r.key()), data, ttl).Err(); err != nil {
		slog.Warn("streamrevoke redis set failed", "error", err, "kind", r.Kind, "id", r.ID)
	}
}

// RevokeSession revokes a single session id for DefaultTTL.
func (s *Store) RevokeSession(ctx context.Context, sessionID, reason string) error {
	return s.Revoke(ctx, Key{Kind: KindSession, ID: sessionID}, reason, time.Now().Add(s.defaultTTL))
}

// RevokeSessionFor revokes a single session id for a caller-chosen TTL. The
// async enforcer uses a short TTL so a transient over-count (e.g. a ghost
// session lingering next to a fresh reconnect) self-heals; a persistent abuser
// is simply re-revoked on the next evaluation pass. ttl <= 0 falls back to
// DefaultTTL.
func (s *Store) RevokeSessionFor(ctx context.Context, sessionID, reason string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = s.defaultTTL
	}
	return s.Revoke(ctx, Key{Kind: KindSession, ID: sessionID}, reason, time.Now().Add(ttl))
}

// RevokeUser revokes the user's streams for DefaultTTL. It is a CUTOFF, not a
// ban: enforcement (IsRevoked) only matches streams whose credential was issued
// before this revocation, so playback the user starts after re-authenticating
// is unaffected. This is what makes it safe for OnUserSessionsRevoked to call
// on every auth-session revocation (admin edits included), while still cutting
// every stream that rode a pre-revocation credential.
func (s *Store) RevokeUser(ctx context.Context, userID int, reason string) error {
	// IsRevoked never matches userID <= 0 against a KindUser entry, so a kill for
	// such an id would be silently ineffective. Refuse it rather than report a
	// success that has no teeth.
	if userID <= 0 {
		return fmt.Errorf("streamrevoke: invalid userID %d", userID)
	}
	return s.Revoke(ctx, userKey(userID), reason, time.Now().Add(s.defaultTTL))
}

// List returns the currently-active revocations, pruning expired entries on
// read.
func (s *Store) List() []Revocation {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Revocation, 0, len(s.items))
	for k, r := range s.items {
		if r.expired(now) {
			delete(s.items, k)
			continue
		}
		out = append(out, r)
	}
	return out
}

// pruneExpired removes expired entries from the in-memory cache.
func (s *Store) pruneExpired() {
	now := time.Now()
	s.mu.Lock()
	for k, r := range s.items {
		if r.expired(now) {
			delete(s.items, k)
		}
	}
	s.mu.Unlock()
}

// StartSync warms the cache (durable store then a Redis SCAN of silo:revoked:*)
// and subscribes to cache.ChannelAdmin to apply EventStreamRevoked events. The
// initial warm and subscribe run SYNCHRONOUSLY (inline) before StartSync
// returns, so kills recorded durably or in Redis are already enforced before
// the first stream is served. Only the PollInterval reconcile/prune loop runs
// in a spawned goroutine. With a nil Redis client the Store is memory-only and
// only the local prune (plus durable maintenance, if configured) runs on the
// tick.
func (s *Store) StartSync(ctx context.Context) {
	// Warm from the durable mirror first so kills survive a Redis flush. Each
	// non-expired entry is also re-armed into Redis (mirrorToRedis no-ops when
	// Redis is absent) so edge nodes — which learn kills only from Redis —
	// reconverge via their SCAN reconcile after a Redis flush + central restart.
	if s.durable != nil {
		if revs, err := s.warmFromDurable(ctx); err != nil {
			slog.Warn("streamrevoke durable warm failed", "error", err)
		} else {
			now := time.Now()
			for _, r := range revs {
				if !r.expired(now) {
					s.applyLocal(r)
					s.mirrorToRedis(ctx, r)
				}
			}
		}
	}

	// Warm from Redis and subscribe for push updates.
	if s.rdb != nil {
		s.reconcileFromRedis(ctx)
	}
	if s.bus != nil {
		if err := s.bus.Subscribe(ctx, cache.ChannelAdmin, s.handleEvent); err != nil {
			slog.Warn("streamrevoke subscribe failed", "error", err)
		}
	}

	go func() {
		ticker := time.NewTicker(s.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.maintain(ctx)
			}
		}
	}()
}

// warmFromDurable loads the active revocations for the boot-time cache warm,
// retrying a bounded number of times with short backoff. A single transient DB
// error at startup (with Redis also flushed) would otherwise leave the kill
// list empty until the first poll tick 60s later; the retry closes that window.
// It is strictly bounded and honors ctx cancellation, so startup never blocks
// indefinitely. Only the boot warm needs this — the recurring maintain tick is
// its own backstop.
func (s *Store) warmFromDurable(ctx context.Context) ([]Revocation, error) {
	backoffs := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}
	var revs []Revocation
	var err error
	for attempt := 0; ; attempt++ {
		if revs, err = s.durable.ListActive(ctx); err == nil {
			return revs, nil
		}
		if attempt >= len(backoffs) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(backoffs[attempt]):
		}
	}
}

// maintain runs one reconcile/prune pass: it is the body of the poll tick,
// factored out so tests can exercise it directly without a live ticker. All
// steps are best-effort — a failure in one is logged and does not block the
// others or the next tick.
func (s *Store) maintain(ctx context.Context) {
	if s.rdb != nil {
		s.reconcileFromRedis(ctx)
	}
	// Durable maintenance: re-warm from the durable mirror so a mid-life Redis
	// flush self-heals, re-arming Redis for edge nodes too, then physically
	// reclaim expired rows.
	if s.durable != nil {
		if revs, err := s.durable.ListActive(ctx); err != nil {
			slog.Warn("streamrevoke durable reconcile failed", "error", err)
		} else {
			now := time.Now()
			for _, r := range revs {
				if !r.expired(now) {
					s.applyLocal(r)
					s.mirrorToRedis(ctx, r)
				}
			}
		}
		if err := s.durable.Prune(ctx); err != nil {
			slog.Warn("streamrevoke durable prune failed", "error", err)
		}
	}
	s.pruneExpired()
}

// handleEvent applies an EventStreamRevoked event to the local cache. Other
// event types on the channel are ignored.
func (s *Store) handleEvent(evt cache.Event) {
	if evt.Type != EventStreamRevoked {
		return
	}
	var r Revocation
	if err := json.Unmarshal([]byte(evt.Payload), &r); err != nil {
		slog.Debug("streamrevoke event unmarshal failed", "error", err)
		return
	}
	if r.expired(time.Now()) {
		return
	}
	s.applyLocal(r)
}

// reconcileFromRedis SCANs the revocation namespace and applies every live
// entry to the local cache. Redis is authoritative for entries it holds; TTL
// expiry there is mirrored by prune-on-read locally.
func (s *Store) reconcileFromRedis(ctx context.Context) {
	var cursor uint64
	now := time.Now()
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, scanPattern, 256).Result()
		if err != nil {
			slog.Warn("streamrevoke redis scan failed", "error", err)
			return
		}
		for _, k := range keys {
			val, err := s.rdb.Get(ctx, k).Result()
			if err != nil {
				// Key may have expired between SCAN and GET; skip it.
				continue
			}
			var r Revocation
			if err := json.Unmarshal([]byte(val), &r); err != nil {
				slog.Debug("streamrevoke redis unmarshal failed", "error", err, "key", k)
				continue
			}
			if !r.expired(now) {
				s.applyLocal(r)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
}
