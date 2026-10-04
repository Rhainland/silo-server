package middleware

import (
	"log/slog"
	"net/http"

	"github.com/Silo-Server/silo-server/internal/access"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// HouseholdProfileGate closes the profile-less path on the frozen v1 viewer
// routes. Those routes register RequireViewerAccess without RequireProfile, so
// a request with no X-Profile-Id resolves to the account's own limits. On an
// account with a PIN-protected or access-restricted profile that is broader
// than the household allows: a device signed into the account could bypass
// the child profile's limits, or the parent profile's PIN, by omitting the
// header. v2 refuses the same request; this is the v1 bridge's critical fix.
type HouseholdProfileGate struct {
	stores userstore.UserStoreProvider
}

// NewHouseholdProfileGate builds the gate over the per-account profile store.
func NewHouseholdProfileGate(stores userstore.UserStoreProvider) *HouseholdProfileGate {
	return &HouseholdProfileGate{stores: stores}
}

// Require answers a request without X-Profile-Id with the v1 400 that
// RequireProfile writes when any profile on the account is limited (see
// access.HouseholdRequiresProfile). A request naming a profile passes: the
// RequireViewerAccess that runs before this gate has already resolved and
// verified it. An account whose profiles are all unlimited keeps account
// scope, so legacy single-profile clients are unaffected. API keys keep their
// profile-less access: only server admins mint them and their own scopes
// bound what they reach. Without a store the gate is a pass-through, matching
// the router, which wires no viewer access without one either.
func (g *HouseholdProfileGate) Require(next http.Handler) http.Handler {
	if g == nil || g.stores == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Profile-Id") != "" {
			next.ServeHTTP(w, r)
			return
		}
		claims := GetClaims(r.Context())
		if claims == nil {
			writeUnauthorized(w, "Authentication required", ReasonAuthenticationRequired)
			return
		}
		if claims.TokenType == auth.TokenTypeAPIKey {
			next.ServeHTTP(w, r)
			return
		}

		store, err := g.stores.ForUser(r.Context(), claims.UserID)
		if err != nil {
			slog.ErrorContext(r.Context(), "household profile gate: opening user store", "component", "api", "user_id", claims.UserID, "error", err)
			writeInternalError(w, "Failed to resolve viewer access")
			return
		}
		profiles, err := store.ListProfiles(r.Context())
		if err != nil {
			slog.ErrorContext(r.Context(), "household profile gate: listing profiles", "component", "api", "user_id", claims.UserID, "error", err)
			writeInternalError(w, "Failed to resolve viewer access")
			return
		}
		if access.HouseholdRequiresProfile(profiles) {
			writeProfileHeaderRequired(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}
