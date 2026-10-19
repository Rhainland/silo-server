package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
)

// connectInfoUsers serves one account by id; the other repository methods are
// never reached by the connect-info handler.
type connectInfoUsers struct {
	UserRepository
	user *models.User
}

func (r connectInfoUsers) GetByID(_ context.Context, id int) (*models.User, error) {
	if r.user == nil || r.user.ID != id {
		return nil, auth.ErrNotFound
	}
	return r.user, nil
}

func TestCompatConnectInfoPasswordLoginAvailable(t *testing.T) {
	passwordAccount := models.User{ID: 7, LocalPasswordLoginEnabled: true, PasswordHash: "$2a$hash"}
	providerAccount := models.User{ID: 7, PasswordHash: "$2a$unusable"}
	breakGlass := passwordAccount
	breakGlass.BreakGlass = true
	noHash := passwordAccount
	noHash.PasswordHash = ""

	tests := []struct {
		name       string
		user       models.User
		localLogin string
		want       bool
	}{
		{name: "password account", user: passwordAccount, want: true},
		{name: "password account, switch on", user: passwordAccount, localLogin: "true", want: true},
		{name: "provider account", user: providerAccount, want: false},
		{name: "password account, switch off", user: passwordAccount, localLogin: "false", want: false},
		{name: "break-glass account, switch off", user: breakGlass, localLogin: "false", want: true},
		{name: "account without a password", user: noHash, want: false},
		{name: "unreadable switch counts as on", user: passwordAccount, localLogin: "maybe", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := &fakeServerSettingsStore{values: map[string]string{}}
			if tt.localLogin != "" {
				settings.values[config.AuthLocalPasswordLoginSettingKey] = tt.localLogin
			}
			user := tt.user
			handler := NewCompatConnectInfoHandler(&config.Config{}, settings, connectInfoUsers{user: &user})
			ctx := middleware.SetClaims(t.Context(), &auth.Claims{UserID: 7})

			got := handler.GetConnectInfo(ctx).Account.PasswordLoginAvailable
			if got != tt.want {
				t.Fatalf("password_login_available = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompatConnectInfoPasswordLoginAvailableDefaultsOnUnknownAccount(t *testing.T) {
	handler := NewCompatConnectInfoHandler(&config.Config{}, &fakeServerSettingsStore{}, connectInfoUsers{})
	ctx := middleware.SetClaims(t.Context(), &auth.Claims{UserID: 7})

	if !handler.GetConnectInfo(ctx).Account.PasswordLoginAvailable {
		t.Fatal("an account that cannot be looked up should keep the password instructions")
	}
}
