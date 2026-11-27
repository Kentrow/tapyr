package collector

import (
	"strings"
	"testing"

	"github.com/kentrow/tapyr/internal/config"
)

func TestReauthProblemNamesLogin(t *testing.T) {
	authCode := &config.Provider{Metadata: config.ProviderMeta{Name: "athletes"}, Spec: config.ProviderSpec{Auth: &config.Auth{
		Type:                    config.AuthOAuth2AuthorizationCode,
		OAuth2AuthorizationCode: &config.OAuth2AuthorizationCode{RefreshToken: config.Value{Secret: "refreshToken"}},
	}}}
	plugin := &config.Provider{Metadata: config.ProviderMeta{Name: "central"}, Spec: config.ProviderSpec{Auth: &config.Auth{Type: config.AuthPlugin}}}
	account := func(name, provider string) *config.Account {
		return &config.Account{Metadata: config.ObjectMeta{Name: config.Name(name)}, Spec: config.AccountSpec{Provider: config.Name(provider)}}
	}
	cfg := &config.Config{
		Providers: []*config.Provider{authCode, plugin},
		Accounts:  []*config.Account{account("home", "athletes"), account("campus", "central")},
	}
	if got := reauthProblem(cfg, "home").Action; !strings.Contains(got, "tapyr auth login home") || !strings.Contains(got, "refreshToken") {
		t.Errorf("oauth2_authorization_code account: %q", got)
	}
	if got := reauthProblem(cfg, "campus").Action; strings.Contains(got, "auth login") {
		t.Errorf("plugin account: %q", got)
	}
}
