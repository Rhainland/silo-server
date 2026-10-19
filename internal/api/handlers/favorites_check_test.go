package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// The v1 membership checks keep the contract shipped clients read: 204 for a
// member and a bare 404 both for a non-member and for an item the viewer may
// not see. Clients that must not log a 404 for a non-member use the v2
// membership reads instead (issue #952).
func TestV1MembershipChecksKeepLegacyContract(t *testing.T) {
	cases := []struct {
		name  string
		add   func(h *PersonalDataHandler, w http.ResponseWriter, r *http.Request)
		check func(h *PersonalDataHandler, w http.ResponseWriter, r *http.Request)
	}{
		{"favorites", (*PersonalDataHandler).HandleAddFavorite, (*PersonalDataHandler).HandleCheckFavorite},
		{"watchlist", (*PersonalDataHandler).HandleAddToWatchlist, (*PersonalDataHandler).HandleCheckWatchlist},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewPersonalDataHandler(testUserStoreProvider{store: newProfileTestStore(t)}, &fakeHistoryItemRepo{
				items: map[string]*models.MediaItem{
					"movie-1": {ContentID: "movie-1", Type: "movie", Title: "Fight Club"},
					"movie-2": {ContentID: "movie-2", Type: "movie", Title: "Heat"},
					"movie-3": {ContentID: "movie-3", Type: "movie", Title: "Hidden"},
				},
				inaccessible: map[string]bool{"movie-3": true},
			})
			call := func(fn func(h *PersonalDataHandler, w http.ResponseWriter, r *http.Request), method, itemID string) *httptest.ResponseRecorder {
				t.Helper()
				req := withChiParam(newAuthorizedProfileRequestWithRole(method, "/"+tc.name+"/"+itemID, "", "user", "profile-1"), "item_id", itemID)
				rr := httptest.NewRecorder()
				fn(handler, rr, req)
				return rr
			}
			if rr := call(tc.add, http.MethodPut, "movie-1"); rr.Code != http.StatusNoContent {
				t.Fatalf("add: %d %s", rr.Code, rr.Body.String())
			}
			for _, want := range []struct {
				itemID string
				code   int
			}{
				{"movie-1", http.StatusNoContent},
				{"movie-2", http.StatusNotFound},
				{"movie-3", http.StatusNotFound},
				{"movie-missing", http.StatusNotFound},
			} {
				if rr := call(tc.check, http.MethodGet, want.itemID); rr.Code != want.code {
					t.Errorf("check %s: %d, want %d", want.itemID, rr.Code, want.code)
				}
			}
		})
	}
}
