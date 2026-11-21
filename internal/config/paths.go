package config

import "path/filepath"

// resolvePaths makes relative paths written in documents relative to the
// directory of the declaring file.
func resolvePaths(cfg *Config) {
	for _, a := range cfg.Accounts {
		dir := filepath.Dir(a.Source.File)
		resolveSecretMap(dir, a.Spec.Secrets)
		for i := range a.Spec.Targets {
			resolveSecretMap(dir, a.Spec.Targets[i].Secrets)
		}
		if a.Spec.TLS != nil {
			a.Spec.TLS.CAFile = resolvePath(dir, a.Spec.TLS.CAFile)
		}
	}
	for _, o := range cfg.Outputs {
		dir := filepath.Dir(o.Source.File)
		if rw := o.Spec.RemoteWrite; rw != nil {
			resolveHeaderSecrets(dir, rw.Headers)
			if rw.BasicAuth != nil {
				resolveSecret(dir, &rw.BasicAuth.Username)
				resolveSecret(dir, &rw.BasicAuth.Password)
			}
		}
		if otlp := o.Spec.OTLP; otlp != nil {
			resolveHeaderSecrets(dir, otlp.Headers)
		}
		if q := o.Spec.Queue; q != nil && q.Disk != nil {
			q.Disk.Path = resolvePath(dir, q.Disk.Path)
		}
	}
	if s := cfg.Settings; s != nil {
		dir := filepath.Dir(s.Source.File)
		srv := &s.Spec.Server
		if srv.BasicAuth != nil {
			resolveSecret(dir, &srv.BasicAuth.Username)
			resolveSecret(dir, &srv.BasicAuth.Password)
		}
		if srv.TLS != nil {
			srv.TLS.CertFile = resolvePath(dir, srv.TLS.CertFile)
			srv.TLS.KeyFile = resolvePath(dir, srv.TLS.KeyFile)
		}
		s.Spec.State.Path = resolvePath(dir, s.Spec.State.Path)
		for i, p := range s.Spec.ProviderPaths {
			s.Spec.ProviderPaths[i] = resolvePath(dir, p)
		}
	}
}

func resolvePath(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

func resolveSecret(dir string, s *SecretRef) {
	s.File = resolvePath(dir, s.File)
}

func resolveSecretMap(dir string, m map[string]SecretRef) {
	for k, s := range m {
		resolveSecret(dir, &s)
		m[k] = s
	}
}

func resolveHeaderSecrets(dir string, m map[string]StringOrSecret) {
	for _, h := range m {
		if h.Secret != nil {
			resolveSecret(dir, h.Secret)
		}
	}
}
