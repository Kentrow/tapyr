package collector

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// shutdownTimeout bounds the graceful shutdown of HTTP servers.
const shutdownTimeout = 10 * time.Second

// Serve serves h on ln until ctx is done, then shuts the server down
// gracefully; with HTTPS when certFile and keyFile are set.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, certFile, keyFile string) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if certFile != "" {
			errc <- srv.ServeTLS(ln, certFile, keyFile)
			return
		}
		errc <- srv.Serve(ln)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// basicAuth protects the status API (and the UI) with HTTP basic
// authentication, and /metrics and /metrics/self when protectMetrics is set.
// Health endpoints are never protected. Credentials are compared in
// constant time.
func basicAuth(next http.Handler, username, password string, protectMetrics bool) http.Handler {
	wantUser, wantPass := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		protected := strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/ui") || path == "/" ||
			(protectMetrics && (path == "/metrics" || path == "/metrics/self"))
		if !protected {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		gotUser, gotPass := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
		userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:]) == 1
		passOK := subtle.ConstantTimeCompare(gotPass[:], wantPass[:]) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="tapyr", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
