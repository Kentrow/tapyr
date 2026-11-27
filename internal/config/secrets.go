package config

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// SecretSource reads secret values. Production code uses the process
// environment and the file system; tests inject fakes.
type SecretSource struct {
	LookupEnv func(name string) (string, bool)
	ReadFile  func(path string) ([]byte, error)
	// OnResolve, when set, receives every value resolved, so that logs
	// redact it.
	OnResolve func(value string)
}

// loginSecret returns the secret of an account that tapyr auth login writes:
// the bootstrap refresh token of an oauth2_authorization_code provider.
func loginSecret(cfg *Config, a *Account) string {
	for _, p := range cfg.Providers {
		if p.Metadata.Name != a.Spec.Provider || p.Spec.Auth == nil {
			continue
		}
		if o := p.Spec.Auth.OAuth2AuthorizationCode; p.Spec.Auth.Type == AuthOAuth2AuthorizationCode && o != nil {
			return o.RefreshToken.Secret
		}
	}
	return ""
}

// CheckSecrets resolves every secret reference of the configuration and
// reports the ones that are missing or empty. Secret values are never
// included in the diagnostics.
func CheckSecrets(cfg *Config, src SecretSource) Diagnostics {
	var diags Diagnostics
	hint := ""
	check := func(s *Source, ref SecretRef, tokens ...string) {
		var problem string
		switch {
		case ref.Env != "":
			v, ok := src.LookupEnv(ref.Env)
			switch {
			case !ok:
				problem = fmt.Sprintf("environment variable %s is not set", ref.Env)
			case strings.TrimSpace(v) == "":
				problem = fmt.Sprintf("environment variable %s is empty", ref.Env)
			}
		case ref.File != "":
			b, err := src.ReadFile(ref.File)
			switch {
			case err != nil:
				problem = fmt.Sprintf("secret file %s: %s", ref.File, secretFileError(err))
			case strings.TrimSpace(string(b)) == "":
				problem = fmt.Sprintf("secret file %s is empty", ref.File)
			}
		}
		if problem != "" {
			problem += hint
			diags = append(diags, Diagnostic{
				Pos:     s.positions.position(tokens, false),
				Path:    s.positions.path(tokens),
				Message: problem,
			})
		}
	}
	for _, a := range cfg.Accounts {
		login := loginSecret(cfg, a)
		for _, k := range sortedKeys(a.Spec.Secrets) {
			if k == login {
				hint = fmt.Sprintf(" (run tapyr auth login %s to write it)", a.Metadata.Name)
			}
			check(a.Source, a.Spec.Secrets[k], "spec", "secrets", k)
			hint = ""
		}
		for i, t := range a.Spec.Targets {
			for _, k := range sortedKeys(t.Secrets) {
				check(a.Source, t.Secrets[k], "spec", "targets", strconv.Itoa(i), "secrets", k)
			}
		}
	}
	for _, o := range cfg.Outputs {
		headers := map[string]StringOrSecret{}
		block := ""
		switch {
		case o.Spec.RemoteWrite != nil:
			block, headers = "remoteWrite", o.Spec.RemoteWrite.Headers
			if ba := o.Spec.RemoteWrite.BasicAuth; ba != nil {
				check(o.Source, ba.Username, "spec", block, "basicAuth", "username")
				check(o.Source, ba.Password, "spec", block, "basicAuth", "password")
			}
		case o.Spec.OTLP != nil:
			block, headers = "otlp", o.Spec.OTLP.Headers
		}
		for _, k := range sortedKeys(headers) {
			if h := headers[k]; h.Secret != nil {
				check(o.Source, *h.Secret, "spec", block, "headers", k)
			}
		}
	}
	if s := cfg.Settings; s != nil && s.Source != nil {
		if ba := s.Spec.Server.BasicAuth; ba != nil {
			check(s.Source, ba.Username, "spec", "server", "basicAuth", "username")
			check(s.Source, ba.Password, "spec", "server", "basicAuth", "password")
		}
	}
	return diags.Sorted()
}

func secretFileError(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "no such file"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return fileError(err)
}
