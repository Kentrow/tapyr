// Package auth applies provider authentication to requests (see
// docs/spec/05-collection.md, "Auth"): the static types none, header,
// bearer and basic (New), and the token types oauth2_client_credentials and
// session_login, whose sessions a Manager shares between jobs.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/secret"
)

// Authenticator decorates requests with credentials.
type Authenticator interface {
	// Decorate adds credentials to the request, obtaining or refreshing
	// tokens if needed.
	Decorate(ctx context.Context, req *http.Request) error
	// OnAuthFailure is called on a response classified as authFailed. It
	// returns true if the request should be retried once after
	// re-authentication.
	OnAuthFailure(ctx context.Context, resp *http.Response) (retry bool, err error)
}

// Secrets are the resolved secrets of an account (or target), by name.
type Secrets map[string]secret.Value

// ErrUnsupported is returned for auth types not available yet.
var ErrUnsupported = errors.New("auth type not supported yet")

// New returns the authenticator of a provider auth block. A nil block is
// type none.
func New(a *config.Auth, secrets Secrets) (Authenticator, error) {
	if a == nil || a.Type == config.AuthNone {
		return none{}, nil
	}
	switch a.Type {
	case config.AuthHeader:
		v, err := resolve(a.Header.Value, secrets)
		if err != nil {
			return nil, fmt.Errorf("auth header value: %w", err)
		}
		return header{name: a.Header.Name, value: secret.New(a.Header.Formatted(v.Reveal()))}, nil
	case config.AuthBearer:
		v, err := resolve(a.Bearer.Token, secrets)
		if err != nil {
			return nil, fmt.Errorf("auth bearer token: %w", err)
		}
		return header{name: "Authorization", value: secret.New("Bearer " + v.Reveal())}, nil
	case config.AuthBasic:
		user, err := resolve(a.Basic.Username, secrets)
		if err != nil {
			return nil, fmt.Errorf("auth basic username: %w", err)
		}
		pass, err := resolve(a.Basic.Password, secrets)
		if err != nil {
			return nil, fmt.Errorf("auth basic password: %w", err)
		}
		return basic{user: user, pass: pass}, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, a.Type)
}

// resolve returns the value of a literal or a secret reference.
func resolve(v config.Value, secrets Secrets) (secret.Value, error) {
	switch {
	case v.Secret != "":
		s, ok := secrets[v.Secret]
		if !ok || s.IsZero() {
			return secret.Value{}, fmt.Errorf("secret %q is not provided by the account", v.Secret)
		}
		return s, nil
	case v.Expr != "":
		return secret.Value{}, fmt.Errorf("%w: expressions in auth values", ErrUnsupported)
	}
	return secret.New(v.Literal), nil
}

type none struct{}

func (none) Decorate(context.Context, *http.Request) error { return nil }

func (none) OnAuthFailure(context.Context, *http.Response) (bool, error) { return false, nil }

type header struct {
	name  string
	value secret.Value
}

func (h header) Decorate(_ context.Context, req *http.Request) error {
	req.Header.Set(h.name, h.value.Reveal())
	return nil
}

func (header) OnAuthFailure(context.Context, *http.Response) (bool, error) { return false, nil }

type basic struct{ user, pass secret.Value }

func (b basic) Decorate(_ context.Context, req *http.Request) error {
	req.SetBasicAuth(b.user.Reveal(), b.pass.Reveal())
	return nil
}

func (basic) OnAuthFailure(context.Context, *http.Response) (bool, error) { return false, nil }
