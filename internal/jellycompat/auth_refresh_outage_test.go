package jellycompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/auth"
)

// A compat session whose Silo tokens are due for refresh while the session
// store is unreachable must survive: the request is a retryable 503 and the
// session stays in the store. A refresh token the server refuses still ends
// the compat session.
func TestRequireSession_StoreOutageKeepsTheSession(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://silo@127.0.0.1:1/silo?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	jwt := auth.NewJWTService("compat-outage-secret", 15*time.Minute, 24*time.Hour)
	svc := auth.NewService(nil, jwt, auth.NewSessionRepository(pool), auth.NewUserRepository(pool), nil, nil, nil)
	refresh, err := jwt.GenerateRefreshToken(1, "user", "sess-1")
	if err != nil {
		t.Fatal(err)
	}

	now := fixedNow()
	clock := func() time.Time { return now }
	store := NewSessionStore(30*24*time.Hour, clock)
	for token, refreshToken := range map[string]string{"outage-tok": refresh, "refused-tok": "not-a-jwt"} {
		_ = store.Put(Session{
			Token:                 token,
			StreamAppUserID:       1,
			StreamAppAccessToken:  "old-access",
			StreamAppRefreshToken: refreshToken,
			// Within the refresh buffer, so the request refreshes first.
			StreamAppTokenExpiry: now.Add(3 * time.Minute),
		})
	}
	authn := &Authenticator{sessions: store, refresher: svc, now: clock}
	handler := authn.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("the request was served without a refreshed session")
	}))
	serve := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
		req.Header.Set("X-Emby-Token", token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := serve("outage-tok")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("store outage = %d Retry-After=%q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if _, ok := store.Get("outage-tok"); !ok {
		t.Fatal("a store outage deleted the compat session")
	}

	if rec := serve("refused-tok"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("refused refresh = %d, want 401", rec.Code)
	}
	if _, ok := store.Get("refused-tok"); ok {
		t.Fatal("a refused refresh kept the compat session")
	}
}

// providerDownRefresher answers every refresh like a sign-in provider that
// could not be reached under the fail_closed outage policy.
type providerDownRefresher struct{}

func (providerDownRefresher) Refresh(context.Context, string) (*auth.TokenPair, error) {
	return nil, fmt.Errorf("re-checking provider identity: %w", auth.ErrProviderUnavailable)
}

// A provider outage refuses this refresh only; it must not end the compat
// session, the same as a native client keeps its session on a 503.
func TestRequireSession_ProviderOutageKeepsTheSession(t *testing.T) {
	now := fixedNow()
	clock := func() time.Time { return now }
	store := NewSessionStore(30*24*time.Hour, clock)
	_ = store.Put(Session{
		Token:                 "provider-tok",
		StreamAppUserID:       1,
		StreamAppRefreshToken: "refresh",
		StreamAppTokenExpiry:  now.Add(3 * time.Minute),
	})
	authn := &Authenticator{sessions: store, refresher: providerDownRefresher{}, now: clock}
	handler := authn.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("the request was served without a refreshed session")
	}))
	req := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	req.Header.Set("X-Emby-Token", "provider-tok")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("provider outage = %d Retry-After=%q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if _, ok := store.Get("provider-tok"); !ok {
		t.Fatal("a provider outage deleted the compat session")
	}
}

// outageSessionRepo is a persistent compat session store whose reads fail
// for the token "outage-tok" and find nothing for any other.
type outageSessionRepo struct{ lookups []string }

func (r *outageSessionRepo) Upsert(context.Context, Session) error { return nil }

func (r *outageSessionRepo) GetByToken(_ context.Context, token string, _ time.Time) (*Session, error) {
	r.lookups = append(r.lookups, token)
	if token == "outage-tok" {
		return nil, errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	}
	return nil, ErrSessionNotFound
}

func (r *outageSessionRepo) DeleteByToken(context.Context, string) error { return nil }

// A node that has not cached a compat session reads it from the database.
// When that read fails, the token was not judged: the request is a
// retryable 503, not the 401 that signs a Jellyfin client out. A token with
// no session, or one Postgres cannot take as text, stays 401.
func TestRequireSession_UncachedSessionStoreOutageIsRetryable(t *testing.T) {
	now := fixedNow()
	repo := &outageSessionRepo{}
	store := NewPersistentSessionStore(30*24*time.Hour, func() time.Time { return now }, repo)
	authn := &Authenticator{sessions: store, now: func() time.Time { return now }}
	handler := authn.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("the request was served without a session")
	}))
	serve := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
		req.Header.Set("X-Emby-Token", token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := serve("outage-tok"); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("store outage = %d Retry-After=%q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := serve("unknown-tok"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token = %d, want 401", rec.Code)
	}
	if rec := serve("bad\xfftok"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token with invalid UTF-8 = %d, want 401", rec.Code)
	}
	for _, token := range repo.lookups {
		if token == "bad\xfftok" {
			t.Fatal("a token with invalid UTF-8 was sent to the database")
		}
	}
}
