package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/secret"
)

func TestNew(t *testing.T) {
	secrets := Secrets{"key": secret.New("k3y"), "user": secret.New("alice"), "pass": secret.New("pw")}
	tests := []struct {
		name       string
		auth       *config.Auth
		wantHeader map[string]string
		wantErr    string
	}{
		{name: "nil is none", auth: nil, wantHeader: map[string]string{"Authorization": ""}},
		{name: "none", auth: &config.Auth{Type: config.AuthNone}, wantHeader: map[string]string{"Authorization": ""}},
		{
			name:       "header from secret",
			auth:       &config.Auth{Type: config.AuthHeader, Header: &config.HeaderAuth{Name: "X-API-Key", Value: config.Value{Secret: "key"}}},
			wantHeader: map[string]string{"X-Api-Key": "k3y"},
		},
		{
			name: "header with a format",
			auth: &config.Auth{Type: config.AuthHeader, Header: &config.HeaderAuth{
				Name: "Authorization", Value: config.Value{Secret: "key"}, Format: "Token {value}",
			}},
			wantHeader: map[string]string{"Authorization": "Token k3y"},
		},
		{
			name:       "header literal",
			auth:       &config.Auth{Type: config.AuthHeader, Header: &config.HeaderAuth{Name: "X-Client", Value: config.Value{Literal: "tapyr"}}},
			wantHeader: map[string]string{"X-Client": "tapyr"},
		},
		{
			name:       "bearer",
			auth:       &config.Auth{Type: config.AuthBearer, Bearer: &config.BearerAuth{Token: config.Value{Secret: "key"}}},
			wantHeader: map[string]string{"Authorization": "Bearer k3y"},
		},
		{
			name: "basic",
			auth: &config.Auth{Type: config.AuthBasic, Basic: &config.BasicAuth{
				Username: config.Value{Secret: "user"}, Password: config.Value{Secret: "pass"},
			}},
			wantHeader: map[string]string{"Authorization": "Basic YWxpY2U6cHc="},
		},
		{
			name:    "missing secret",
			auth:    &config.Auth{Type: config.AuthBearer, Bearer: &config.BearerAuth{Token: config.Value{Secret: "other"}}},
			wantErr: `auth bearer token: secret "other" is not provided by the account`,
		},
		{
			name:    "expression",
			auth:    &config.Auth{Type: config.AuthHeader, Header: &config.HeaderAuth{Name: "X", Value: config.Value{Expr: "now"}}},
			wantErr: "auth header value: auth type not supported yet: expressions in auth values",
		},
		{
			name:    "token types come later",
			auth:    &config.Auth{Type: config.AuthSessionLogin, SessionLogin: &config.SessionLogin{}},
			wantErr: "auth type not supported yet: session_login",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := New(tt.auth, secrets)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("New() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://api.example.invalid", http.NoBody)
			if err := a.Decorate(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.wantHeader {
				if got := req.Header.Get(k); got != v {
					t.Errorf("header %s = %q, want %q", k, got, v)
				}
			}
			if retry, err := a.OnAuthFailure(context.Background(), &http.Response{StatusCode: http.StatusUnauthorized}); retry || err != nil {
				t.Errorf("OnAuthFailure() = %v, %v", retry, err)
			}
			if strings.Contains(fmt.Sprintf("%v %+v", a, a), "k3y") {
				t.Error("authenticator formatting leaks the secret")
			}
		})
	}
	if _, err := New(&config.Auth{Type: config.AuthPlugin}, nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("plugin error = %v", err)
	}
}
