package jellycompat

import (
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
	authn := &Authenticator{sessions: store, authService: svc, now: clock}
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
