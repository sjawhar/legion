package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"time"
)

// pprofAddress is where DISPATCH_PPROF_PORT asks the profiling listener to bind: 127.0.0.1 and
// that port, so it is reachable only from inside the pod or through `kubectl port-forward`.
// Unset or empty, it is "", and nothing listens. A value that is not a port 1-65535 refuses
// startup naming the variable.
func pprofAddress(env settingValues) (string, error) {
	raw := strings.TrimSpace(env.get("DISPATCH_PPROF_PORT"))
	if raw == "" {
		return "", nil
	}
	port, err := parsePort("DISPATCH_PPROF_PORT", raw)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
}

// pprofHandler serves net/http/pprof on a mux of its own. Importing net/http/pprof also
// registers its handlers on http.DefaultServeMux, which nothing in Dispatch serves.
func pprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// startPprof binds addr and serves pprofHandler on it until ctx ends, when the listener and every
// open connection close at once: a profile in flight is cut off rather than allowed to delay the
// process's shutdown. An empty addr starts nothing and returns a nil address. A bind that fails is
// an error naming DISPATCH_PPROF_PORT, so a port clash refuses the boot.
func startPprof(ctx context.Context, addr string) (net.Addr, error) {
	if addr == "" {
		return nil, nil
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("DISPATCH_PPROF_PORT: listen on %s: %w", addr, err)
	}
	server := &http.Server{Handler: pprofHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("dispatch: pprof listener stopped", "addr", addr, "error", err)
		}
	}()
	slog.Info("dispatch: pprof listening", "addr", listener.Addr().String())
	return listener.Addr(), nil
}
