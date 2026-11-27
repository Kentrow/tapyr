package collector

import (
	"maps"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/buildinfo"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/problem"
	"github.com/kentrow/tapyr/internal/state"
)

// runProblem says in words what went wrong in a run that did not succeed,
// and what to do: nil for a success. The secrets it names are those of
// the account, overridden by those of the target of the run.
func runProblem(cfg *config.Config, account, target string, r state.RunRecord) *api.Problem {
	if r.Result == "success" || r.Result == "" {
		return nil
	}
	class := r.ErrorClass
	if class == "" && r.Result == "skipped_circuit" {
		class = "circuit_open"
	}
	return problem.For(problem.Input{
		Class: class, Status: r.ErrorStatus, Subject: r.ErrorSubject, Until: r.ErrorUntil,
		Account: account, Secrets: accountSecrets(cfg, account, target), Version: buildinfo.Get().Version,
	})
}

// reauthProblem is the problem of an account whose credentials can no
// longer be renewed.
func reauthProblem(cfg *config.Config, account string) *api.Problem {
	return problem.For(problem.Input{
		Class: problem.ClassReauth, Account: account, Secrets: accountSecrets(cfg, account, ""), Version: buildinfo.Get().Version,
		LoginSecret: loginSecret(cfg, account),
	})
}

// loginSecret returns the bootstrap refresh token secret of an
// oauth2_authorization_code account, which tapyr auth login renews, or "".
func loginSecret(cfg *config.Config, account string) string {
	if cfg == nil {
		return ""
	}
	for _, a := range cfg.Accounts {
		if string(a.Metadata.Name) != account {
			continue
		}
		for _, p := range cfg.Providers {
			if o := p.Spec.Auth.OAuth2AuthorizationCode; p.Metadata.Name == a.Spec.Provider && o != nil &&
				p.Spec.Auth.Type == config.AuthOAuth2AuthorizationCode {
				return o.RefreshToken.Secret
			}
		}
	}
	return ""
}

// accountSecrets returns the references of the secrets of an account, with
// those of a target overriding them.
func accountSecrets(cfg *config.Config, account, target string) map[string]config.SecretRef {
	if cfg == nil {
		return nil
	}
	for _, a := range cfg.Accounts {
		if string(a.Metadata.Name) != account {
			continue
		}
		out := maps.Clone(a.Spec.Secrets)
		for _, t := range a.Spec.Targets {
			if target != "" && string(t.Name) == target {
				if out == nil {
					out = map[string]config.SecretRef{}
				}
				maps.Copy(out, t.Secrets)
			}
		}
		return out
	}
	return nil
}
