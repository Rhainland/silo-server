package handlers

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Silo-Server/silo-server/internal/access"
	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// canManageHousehold reports whether the caller may act for the whole household
// — every profile on their own account — rather than only for themselves.
//
// Server admins always may. Otherwise the caller's active profile must be the
// one flagged is_primary, which is the household parent and is deliberately
// *not* the server-wide admin role: a household parent manages their family,
// an admin manages the server.
//
// When that primary profile has a PIN, management additionally requires a valid
// X-Profile-Token from /profiles/{id}/verify-pin. Without that, a client could
// walk past a profile lock by sending only X-Profile-Id.
//
// This is a policy boundary for well-behaved clients rather than a defense
// against the account holder: every profile on an account shares one login
// session, so X-Profile-Id is self-asserted (see the note in
// internal/api/middleware/auth.go). It is the same boundary profile management
// has always used, and it is applied here so household settings management is
// guarded and auditable rather than implicit.
func canManageHousehold(
	r *http.Request,
	store userstore.UserStore,
	tokens *access.ProfileTokenService,
) (bool, error) {
	return canManageHouseholdAs(r.Context(), store, activeProfileIDOf(r), func(profileID string) error {
		return verifyProfileTokenIn(r, store, tokens, profileID)
	})
}

// activeProfileIDOf is the profile the request acts as: the one the profile
// gate resolved, else the declared header.
func activeProfileIDOf(r *http.Request) string {
	if id := apimw.GetProfileID(r.Context()); id != "" {
		return id
	}
	return r.Header.Get("X-Profile-Id")
}

// canManageHouseholdAs is canManageHousehold with the request already reduced
// to the acting profile and a verifier for a PIN-locked primary profile: the
// v1 verifier checks X-Profile-Token, the v2 one the viewer scope the gate
// resolved.
func canManageHouseholdAs(
	ctx context.Context,
	store userstore.UserStore,
	activeProfileID string,
	verify func(profileID string) error,
) (bool, error) {
	if apimw.IsAdmin(ctx) {
		return true, nil
	}
	if activeProfileID == "" {
		return false, nil
	}
	active, err := store.GetProfile(ctx, activeProfileID)
	if err != nil {
		return false, err
	}
	if active == nil {
		return false, nil
	}
	if !active.IsPrimary {
		return false, nil
	}
	if active.PINHash == "" {
		return true, nil
	}
	if err := verify(active.ID); err != nil {
		return false, err
	}
	return true, nil
}

// verifyProfileToken checks the X-Profile-Token a PIN-locked profile must
// present, reading the profile from the caller's own store. Missing
// dependencies fail closed: a handler wired without a token service cannot
// verify a PIN, and "cannot verify" is not "verified".
func verifyProfileToken(
	r *http.Request,
	stores userstore.UserStoreProvider,
	tokens *access.ProfileTokenService,
	profileID string,
) error {
	if stores == nil || tokens == nil {
		return access.ErrProfileUnverified
	}
	userID := apimw.GetUserID(r.Context())
	if userID == 0 {
		return access.ErrProfileUnverified
	}
	store, err := stores.ForUser(r.Context(), userID)
	if err != nil {
		return fmt.Errorf("opening user store: %w", err)
	}
	return verifyProfileTokenIn(r, store, tokens, profileID)
}

// verifyProfileTokenIn is verifyProfileToken against an already opened store
// for the calling account. The token must match the profile's current PIN
// revision (see access.CheckProfileToken).
func verifyProfileTokenIn(
	r *http.Request,
	store userstore.UserStore,
	tokens *access.ProfileTokenService,
	profileID string,
) error {
	if store == nil || tokens == nil {
		return access.ErrProfileUnverified
	}

	claims := apimw.GetClaims(r.Context())
	if claims == nil || claims.SessionID == "" {
		return access.ErrProfileUnverified
	}

	userID := apimw.GetUserID(r.Context())
	if userID == 0 {
		return access.ErrProfileUnverified
	}

	profile, err := store.GetProfile(r.Context(), profileID)
	if err != nil {
		return fmt.Errorf("loading profile: %w", err)
	}
	if profile == nil {
		return access.ErrProfileUnverified
	}

	return access.CheckProfileToken(tokens, r.Header.Get("X-Profile-Token"), userID, claims.SessionID, profile)
}
