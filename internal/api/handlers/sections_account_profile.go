package handlers

import (
	"context"
	"net/http"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/sections"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// An administrator's access to one profile's page layout on any account. The
// caller does not hold the addressed profile, so every operation first
// confirms that the profile belongs to the addressed account, then runs the
// same read, recipe gate, and write the profile's own routes run. Writes are
// audited the way an administrator's settings write is: identity only.

// profileSectionsAuditKey names the page layout in a settings audit record.
const profileSectionsAuditKey = "profile_sections"

// requireAccountProfile answers 404 not_found unless profileID is a profile of
// the account userID.
func (h *SectionHandler) requireAccountProfile(ctx context.Context, userID int, profileID string) error {
	if profileID == "" {
		return apiError(http.StatusNotFound, "not_found", "Profile not found")
	}
	if h.StoreProvider == nil {
		return apiError(http.StatusInternalServerError, "internal_error", "User store not available")
	}
	store, err := h.StoreProvider.ForUser(ctx, userID)
	if err != nil {
		return apiError(http.StatusInternalServerError, "internal_error", "Failed to access user store")
	}
	profile, err := store.GetProfile(ctx, profileID)
	if err != nil {
		return apiError(http.StatusInternalServerError, "internal_error", "Failed to load profile")
	}
	if profile == nil {
		return apiError(http.StatusNotFound, "not_found", "Profile not found")
	}
	return nil
}

// ListAccountProfileOverrides is ListProfileOverrides for a profile of any
// account.
func (h *SectionHandler) ListAccountProfileOverrides(ctx context.Context, q SectionOverridesQuery) ([]userstore.SectionOverride, error) {
	if err := h.requireAccountProfile(ctx, q.UserID, q.ProfileID); err != nil {
		return nil, err
	}
	return h.ListProfileOverrides(ctx, q)
}

// ResolveAccountProfileSectionSettings is ResolveProfileSectionSettings for a
// profile of any account. It applies no viewer filter: the administrator sees
// every section the profile's layout orders, including ones the account's
// library access currently hides, so a full-replacement save keeps them.
func (h *SectionHandler) ResolveAccountProfileSectionSettings(ctx context.Context, q SectionOverridesQuery, libraryID *int) ([]sections.ResolvedSection, error) {
	if err := h.requireAccountProfile(ctx, q.UserID, q.ProfileID); err != nil {
		return nil, err
	}
	return h.ResolveProfileSectionSettings(ctx, q.UserID, q.ProfileID, q.Scope, libraryID, catalog.AccessFilter{UserID: q.UserID, ProfileID: q.ProfileID})
}

// SaveAccountProfileOverrides is SaveProfileOverrides for a profile of any
// account, audited.
func (h *SectionHandler) SaveAccountProfileOverrides(ctx context.Context, q SectionOverridesQuery, writes []SectionOverrideWrite) error {
	if err := h.requireAccountProfile(ctx, q.UserID, q.ProfileID); err != nil {
		return err
	}
	if err := h.SaveProfileOverrides(ctx, q, writes); err != nil {
		return err
	}
	auditProfileSections(ctx, settingsAuditActionSet, q)
	return nil
}

// ResetAccountProfileOverrides is ResetProfileOverrides for a profile of any
// account, audited.
func (h *SectionHandler) ResetAccountProfileOverrides(ctx context.Context, q SectionOverridesQuery) error {
	if err := h.requireAccountProfile(ctx, q.UserID, q.ProfileID); err != nil {
		return err
	}
	if err := h.ResetProfileOverrides(ctx, q); err != nil {
		return err
	}
	auditProfileSections(ctx, settingsAuditActionClear, q)
	return nil
}

func auditProfileSections(ctx context.Context, action string, q SectionOverridesQuery) {
	auditSettingsForOther(ctx, settingsAuditRecord{
		Action:          action,
		ActorProfileID:  actingProfileID(ctx),
		TargetProfileID: q.ProfileID,
		TargetUserID:    q.UserID,
		Key:             profileSectionsAuditKey,
		Scope:           q.Scope,
		LibraryID:       q.LibraryID,
	})
}
