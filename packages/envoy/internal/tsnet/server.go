package tsnet

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"tailscale.com/tsnet"
)

// Server wraps tsnet.Server with Envoy-specific defaults and provides
// helpers for creating HTTP servers on the Tailscale network.
type Server struct {
	ts *tsnet.Server
}

// New creates a tsnet Server from the given Config. AuthKey may be a
// direct Tailscale auth key or an OAuth-derived key (see resolveAuthKey).
// The caller must call Close when finished.
func New(cfg Config) *Server {
	ts := &tsnet.Server{
		Hostname: cfg.Hostname,
		Dir:      cfg.StateDir,
		AuthKey:  cfg.AuthKey,
		// Silence verbose internal logs (WireGuard, LocalBackend).
		// Only user-visible logs (auth URL, status) go to the app logger.
		Logf:     func(string, ...any) {},
		UserLogf: log.Printf,
	}
	return &Server{ts: ts}
}

// ListenTLS returns a TLS net.Listener on the Tailscale network at the
// given address (e.g., ":443"). The TLS certificate is automatically
// provisioned via Tailscale's ACME integration.
func (s *Server) ListenTLS(network, addr string) (net.Listener, error) {
	ln, err := s.ts.ListenTLS(network, addr)
	if err != nil {
		return nil, fmt.Errorf("tsnet ListenTLS: %w", err)
	}
	return ln, nil
}

// HTTPServer creates an *http.Server configured for the tsnet TLS
// listener with Envoy-standard timeouts.
func HTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// Serve starts the tsnet node, obtains a TLS listener on :443, and
// serves the given handler. It blocks until the server encounters a
// fatal error or the context is cancelled. Returns the net.Listener
// for external lifecycle management.
func (s *Server) Serve(handler http.Handler, fatal chan<- error) (net.Listener, error) {
	ln, err := s.ListenTLS("tcp", ":443")
	if err != nil {
		return nil, err
	}
	srv := HTTPServer(handler)
	go func() {
		log.Printf("tsnet serving on %s:443", s.ts.Hostname)
		if err := srv.Serve(ln); err != http.ErrServerClosed {
			fatal <- fmt.Errorf("tsnet serve: %w", err)
		}
	}()
	return ln, nil
}

// Close shuts down the tsnet server and releases all resources.
// Idempotent — safe to call multiple times (returns net.ErrClosed on
// subsequent calls).
func (s *Server) Close() error {
	return s.ts.Close()
}

// Hostname returns the configured Tailscale hostname.
func (s *Server) Hostname() string {
	return s.ts.Hostname
}
