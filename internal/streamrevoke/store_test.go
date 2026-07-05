package streamrevoke

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// errFakeDurable is the error fakeDurable.ListActive returns while failNext > 0.
var errFakeDurable = errors.New("fake durable failure")

// newMemStore returns a memory-only Store (no Redis, no bus, no durable mirror).
func newMemStore() *Store {
	return New(Options{})
}

// fakeDurable is an in-memory DurableStore double for exercising the durable
// wiring (Upsert on revoke, warm on start, Prune on the poll tick) without a
// live Postgres. StartSync's warm is synchronous, but the poll goroutine calls
// ListActive/Prune concurrently with the test's own reads, so every field is
// mutex-guarded. failNext, when > 0, makes ListActive return an error that many
// times before succeeding, to exercise the bounded boot-warm retry.
type fakeDurable struct {
	mu       sync.Mutex
	rows     map[Key]Revocation
	upserts  int
	prunes   int
	failNext int
	lists    int
}

func newFakeDurable() *fakeDurable {
	return &fakeDurable{rows: make(map[Key]Revocation)}
}

func (f *fakeDurable) Upsert(_ context.Context, r Revocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[r.key()] = r
	f.upserts++
	return nil
}

func (f *fakeDurable) ListActive(_ context.Context) ([]Revocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.failNext > 0 {
		f.failNext--
		return nil, errFakeDurable
	}
	now := time.Now()
	out := make([]Revocation, 0, len(f.rows))
	for _, r := range f.rows {
		if !r.expired(now) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeDurable) Prune(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for k, r := range f.rows {
		if r.expired(now) {
			delete(f.rows, k)
		}
	}
	f.prunes++
	return nil
}

func (f *fakeDurable) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upserts
}

func (f *fakeDurable) pruneCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prunes
}

func (f *fakeDurable) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

// TestRevokeMirrorsToDurable asserts Revoke writes through to the durable store.
func TestRevokeMirrorsToDurable(t *testing.T) {
	ctx := context.Background()
	fake := newFakeDurable()
	s := New(Options{Durable: fake})

	if err := s.RevokeSession(ctx, "sess-1", "abuse"); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if got := fake.upsertCount(); got != 1 {
		t.Fatalf("durable upserts = %d, want 1", got)
	}
	// Re-revoking the same session upserts the same row (bounded growth).
	if err := s.RevokeSession(ctx, "sess-1", "abuse again"); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	fake.mu.Lock()
	rows := len(fake.rows)
	fake.mu.Unlock()
	if rows != 1 {
		t.Fatalf("durable rows after re-revoke = %d, want 1", rows)
	}
}

// TestStartSyncWarmsFromDurable simulates a restart: a fresh Store must
// repopulate its hot-path map from the durable mirror, and skip expired rows.
func TestStartSyncWarmsFromDurable(t *testing.T) {
	// Cancel the poll goroutine StartSync spawns so it does not leak past the test.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	fake := newFakeDurable()
	// Seed the durable store as if a previous process had written these.
	active := Revocation{Kind: KindSession, ID: "sess-live", Reason: "x", RevokedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	expired := Revocation{Kind: KindUser, ID: "9", Reason: "x", RevokedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}
	_ = fake.Upsert(ctx, active)
	_ = fake.Upsert(ctx, expired)

	s := New(Options{Durable: fake})
	s.StartSync(ctx)

	if !s.IsRevoked("sess-live", 0, time.Time{}) {
		t.Fatalf("expected sess-live to be revoked after warm from durable")
	}
	// startedAt predates the (expired) user revocation, so only expiry decides.
	if s.IsRevoked("whatever", 9, time.Now().Add(-3*time.Hour)) {
		t.Fatalf("expected expired user 9 not to be warmed into the cache")
	}
}

// TestWarmFromDurableRetries proves the bounded boot-warm retry (Fix 2): a
// transient ListActive failure at startup is retried rather than leaving the
// kill list empty until the first poll tick.
func TestWarmFromDurableRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	fake := newFakeDurable()
	live := Revocation{Kind: KindSession, ID: "sess-retry", Reason: "x", RevokedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	_ = fake.Upsert(ctx, live)
	// Fail the first two ListActive calls; the third (within the 3-attempt
	// bound) succeeds.
	fake.mu.Lock()
	fake.failNext = 2
	fake.lists = 0
	fake.mu.Unlock()

	s := New(Options{Durable: fake})
	s.StartSync(ctx)

	if !s.IsRevoked("sess-retry", 0, time.Time{}) {
		t.Fatalf("expected sess-retry warmed into cache after retried ListActive")
	}
	if got := fake.listCount(); got != 3 {
		t.Fatalf("ListActive calls = %d, want 3 (two failures + one success)", got)
	}
}

// TestWarmFromDurableGivesUpBounded proves the retry is bounded: when every
// attempt fails, warmFromDurable stops after the fixed attempt budget instead
// of blocking startup.
func TestWarmFromDurableGivesUpBounded(t *testing.T) {
	fake := newFakeDurable()
	fake.mu.Lock()
	fake.failNext = 100 // always fail
	fake.mu.Unlock()

	s := New(Options{Durable: fake})
	if _, err := s.warmFromDurable(context.Background()); err == nil {
		t.Fatalf("expected warmFromDurable to return an error when all attempts fail")
	}
	if got := fake.listCount(); got != 3 {
		t.Fatalf("ListActive attempts = %d, want 3 (bounded)", got)
	}
}

// TestMaintainPrunesAndReWarmsFromDurable asserts the poll-tick body calls the
// durable Prune and re-warms live rows into the hot-path map (self-healing a
// Redis flush that cleared the local cache).
func TestMaintainPrunesAndReWarmsFromDurable(t *testing.T) {
	ctx := context.Background()
	fake := newFakeDurable()
	live := Revocation{Kind: KindSession, ID: "sess-heal", Reason: "x", RevokedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	dead := Revocation{Kind: KindSession, ID: "sess-dead", Reason: "x", RevokedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}
	_ = fake.Upsert(ctx, live)
	_ = fake.Upsert(ctx, dead)

	s := New(Options{Durable: fake})
	// Local cache starts empty (as after a Redis flush + local reconcile miss).
	if s.IsRevoked("sess-heal", 0, time.Time{}) {
		t.Fatalf("did not expect sess-heal in a fresh empty cache")
	}

	s.maintain(ctx)

	if got := fake.pruneCount(); got != 1 {
		t.Fatalf("durable prune calls = %d, want 1", got)
	}
	if !s.IsRevoked("sess-heal", 0, time.Time{}) {
		t.Fatalf("expected sess-heal re-warmed into cache by maintain")
	}
	fake.mu.Lock()
	_, deadStillThere := fake.rows[dead.key()]
	fake.mu.Unlock()
	if deadStillThere {
		t.Fatalf("expected expired durable row to be pruned")
	}
}

func TestIsRevoked(t *testing.T) {
	ctx := context.Background()

	// preCutoff stands in for a stream credential issued before any revocation
	// a test writes; user-kind matching requires the credential to predate the
	// revocation (the cutoff), session-kind matching ignores it.
	preCutoff := time.Now().Add(-time.Minute)

	tests := []struct {
		name       string
		setup      func(t *testing.T, s *Store)
		sessionID  string
		userID     int
		startedAt  time.Time
		wantRevoke bool
	}{
		{
			name: "revoked session is revoked",
			setup: func(t *testing.T, s *Store) {
				if err := s.RevokeSession(ctx, "sess-1", "abuse"); err != nil {
					t.Fatalf("RevokeSession: %v", err)
				}
			},
			sessionID:  "sess-1",
			userID:     42,
			startedAt:  preCutoff,
			wantRevoke: true,
		},
		{
			name: "revoked user revokes that user's pre-revocation streams",
			setup: func(t *testing.T, s *Store) {
				if err := s.RevokeUser(ctx, 7, "banned"); err != nil {
					t.Fatalf("RevokeUser: %v", err)
				}
			},
			sessionID:  "any-session-for-user-7",
			userID:     7,
			startedAt:  preCutoff,
			wantRevoke: true,
		},
		{
			name: "user revocation is a cutoff: a post-revocation credential plays",
			setup: func(t *testing.T, s *Store) {
				if err := s.RevokeUser(ctx, 7, "sessions_revoked"); err != nil {
					t.Fatalf("RevokeUser: %v", err)
				}
			},
			sessionID:  "fresh-session-for-user-7",
			userID:     7,
			startedAt:  time.Now().Add(time.Second),
			wantRevoke: false,
		},
		{
			name: "user revocation never matches an unknown credential time (fail open)",
			setup: func(t *testing.T, s *Store) {
				if err := s.RevokeUser(ctx, 7, "banned"); err != nil {
					t.Fatalf("RevokeUser: %v", err)
				}
			},
			sessionID:  "sess-without-start-time",
			userID:     7,
			startedAt:  time.Time{},
			wantRevoke: false,
		},
		{
			name: "session revocation ignores the credential time",
			setup: func(t *testing.T, s *Store) {
				if err := s.RevokeSession(ctx, "sess-exact", "admin_terminate"); err != nil {
					t.Fatalf("RevokeSession: %v", err)
				}
			},
			sessionID:  "sess-exact",
			userID:     42,
			startedAt:  time.Now().Add(time.Hour),
			wantRevoke: true,
		},
		{
			name: "expired revocation is not revoked",
			setup: func(t *testing.T, s *Store) {
				past := time.Now().Add(-time.Minute)
				if err := s.Revoke(ctx, Key{Kind: KindSession, ID: "sess-old"}, "stale", past); err != nil {
					t.Fatalf("Revoke: %v", err)
				}
			},
			sessionID:  "sess-old",
			userID:     1,
			startedAt:  preCutoff,
			wantRevoke: false,
		},
		{
			name:       "unrelated session and user are not revoked",
			setup:      func(t *testing.T, s *Store) {},
			sessionID:  "unknown",
			userID:     999,
			startedAt:  preCutoff,
			wantRevoke: false,
		},
		{
			name: "unrelated session with a different revoked user is not revoked",
			setup: func(t *testing.T, s *Store) {
				if err := s.RevokeUser(ctx, 7, "banned"); err != nil {
					t.Fatalf("RevokeUser: %v", err)
				}
			},
			sessionID:  "sess-other",
			userID:     8,
			startedAt:  preCutoff,
			wantRevoke: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newMemStore()
			tt.setup(t, s)
			if got := s.IsRevoked(tt.sessionID, tt.userID, tt.startedAt); got != tt.wantRevoke {
				t.Fatalf("IsRevoked(%q, %d, %v) = %v, want %v", tt.sessionID, tt.userID, tt.startedAt, got, tt.wantRevoke)
			}
		})
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()

	if got := s.List(); len(got) != 0 {
		t.Fatalf("List on empty store = %d entries, want 0", len(got))
	}

	if err := s.RevokeSession(ctx, "sess-a", "r1"); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if err := s.RevokeUser(ctx, 5, "r2"); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	// An expired entry must not appear in List.
	if err := s.Revoke(ctx, Key{Kind: KindSession, ID: "sess-exp"}, "r3", time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	got := s.List()
	if len(got) != 2 {
		t.Fatalf("List = %d active entries, want 2: %+v", len(got), got)
	}

	seen := make(map[Key]Revocation, len(got))
	for _, r := range got {
		seen[Key{Kind: r.Kind, ID: r.ID}] = r
	}
	if _, ok := seen[Key{Kind: KindSession, ID: "sess-a"}]; !ok {
		t.Errorf("List missing revoked session sess-a")
	}
	if _, ok := seen[Key{Kind: KindUser, ID: "5"}]; !ok {
		t.Errorf("List missing revoked user 5")
	}
	if _, ok := seen[Key{Kind: KindSession, ID: "sess-exp"}]; ok {
		t.Errorf("List returned expired entry sess-exp")
	}
}

func TestExpiryPrunesFromCache(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()

	// Revoke with a short future TTL, then confirm it lapses.
	until := time.Now().Add(30 * time.Millisecond)
	if err := s.Revoke(ctx, Key{Kind: KindUser, ID: "3"}, "temp", until); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	preCutoff := time.Now().Add(-time.Minute)
	if !s.IsRevoked("whatever", 3, preCutoff) {
		t.Fatalf("expected user 3 to be revoked before expiry")
	}

	time.Sleep(50 * time.Millisecond)

	if s.IsRevoked("whatever", 3, preCutoff) {
		t.Fatalf("expected user 3 to no longer be revoked after expiry")
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("List after expiry = %d entries, want 0", len(got))
	}
}

// TestMonotonicExpiry guards the invariant that a re-revoke with a SHORTER TTL
// can never shorten a longer-lived kill. The async over-cap enforcer re-revokes
// with a short self-healing TTL; without monotonic expiry it would shrink an
// admin's 24h RevokeSession on the same session key and reopen the
// restart-resurrection window.
func TestMonotonicExpiry(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()

	long := time.Now().Add(24 * time.Hour)
	if err := s.Revoke(ctx, Key{Kind: KindSession, ID: "sess-1"}, "admin_terminate", long); err != nil {
		t.Fatalf("Revoke long: %v", err)
	}
	// A shorter re-revoke (the enforcer's 5m self-heal TTL) must not win.
	short := time.Now().Add(5 * time.Minute)
	if err := s.Revoke(ctx, Key{Kind: KindSession, ID: "sess-1"}, "over_cap", short); err != nil {
		t.Fatalf("Revoke short: %v", err)
	}
	got := s.List()
	if len(got) != 1 {
		t.Fatalf("List = %d entries, want 1", len(got))
	}
	if !got[0].ExpiresAt.Equal(long) {
		t.Fatalf("ExpiresAt = %v, want the longer %v (shorter re-revoke must not shorten the kill)", got[0].ExpiresAt, long)
	}

	// A LONGER re-revoke does extend the kill.
	longer := time.Now().Add(48 * time.Hour)
	if err := s.Revoke(ctx, Key{Kind: KindSession, ID: "sess-1"}, "extended", longer); err != nil {
		t.Fatalf("Revoke longer: %v", err)
	}
	got = s.List()
	if len(got) != 1 || !got[0].ExpiresAt.Equal(longer) {
		t.Fatalf("ExpiresAt after longer re-revoke = %v, want %v", got[0].ExpiresAt, longer)
	}
}

// TestUserZeroNeverMatches guards the "no resolved owner" sentinel: a session-
// only check (userID 0, e.g. from the transcode node) must never be caught by a
// stray user:"0" revocation, which would read as "every ownerless request is
// revoked".
func TestUserZeroNeverMatches(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()

	// RevokeUser(0) is rejected outright: IsRevoked never matches a user:0 entry,
	// so creating one would be a silently-ineffective kill. Guarding here means no
	// stray user:0 revocation can exist to be misread as "every ownerless request
	// is revoked".
	if err := s.RevokeUser(ctx, 0, "should-not-nuke-everything"); err == nil {
		t.Fatalf("RevokeUser(0) must be rejected, got nil error")
	}
	if s.IsRevoked("some-unrelated-session", 0, time.Now().Add(-time.Minute)) {
		t.Fatalf("userID 0 sentinel must not match a user:0 revocation")
	}
	// A real session revocation still works with a 0 owner id.
	if err := s.RevokeSession(ctx, "sess-x", "abuse"); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if !s.IsRevoked("sess-x", 0, time.Time{}) {
		t.Fatalf("expected sess-x to be revoked even with owner id 0")
	}
}
