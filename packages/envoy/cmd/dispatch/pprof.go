package main

import (
	"context"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"strconv"
	"strings"
)

// The contention profiles' sampling rates, set only while the profiler listens (startPprof), since
// a profile nobody can take costs for nothing. A process with neither set records no mutex or block
// event, and a load test's question - which lock or channel the goroutines queue on - goes
// unanswered. Both are low enough to leave on in production: one contended mutex unlock in
// mutexProfileFraction is recorded, and a blocking event is sampled with a chance of its duration
// over blockProfileRate nanoseconds, so every block of a millisecond or more is recorded and a
// microsecond's one time in a thousand.
const (
	mutexProfileFraction = 100
	blockProfileRate     = 1_000_000
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
// process's shutdown (serveSide). It turns the mutex and block profiles on first, at
// mutexProfileFraction and blockProfileRate, so /debug/pprof/mutex and /debug/pprof/block record
// from boot on. An empty addr starts nothing, turns on nothing, and returns a nil address. A bind
// that fails is an error naming DISPATCH_PPROF_PORT, so a port clash refuses the boot.
func startPprof(ctx context.Context, addr string) (net.Addr, error) {
	if addr == "" {
		return nil, nil
	}
	runtime.SetMutexProfileFraction(mutexProfileFraction)
	runtime.SetBlockProfileRate(blockProfileRate)
	return serveSide(ctx, addr, "DISPATCH_PPROF_PORT", "pprof", pprofHandler())
}
