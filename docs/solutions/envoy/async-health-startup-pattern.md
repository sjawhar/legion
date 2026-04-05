---
title: "Go async health startup: bind HTTP before slow dependencies"
category: envoy
tags:
  - go
  - health-check
  - startup
  - atomic-pointer
  - readiness-gate
  - nats
  - pulumi
  - net-listen
  - servemux
  - gitignore
date: 2026-04-05
status: active
module: envoy
related_issues:
  - "#235"
symptoms:
  - "Pulumi deploy timeout waiting for health check"
  - "health endpoint unreachable during NATS connection"
  - "503 from /healthz during startup"
  - "container appears down while connecting to NATS"
  - "new test files in cmd/listener/ not tracked by git"
---

# Go Async Health Startup: Bind HTTP Before Slow Dependencies

When a Go service depends on slow external resources (NATS, databases, remote APIs), the
health check endpoint must be reachable before those dependencies initialize. Otherwise
orchestrators (Pulumi, Kubernetes) time out and kill the container.

## The Pattern: 7-Phase Startup

```
1. config.Load()           — synchronous, fast
2. net.Listen("tcp", addr) — bind port in main goroutine (deterministic)
3. Build HTTP mux           — /healthz always available, /v1/* behind readiness gate
4. go server.Serve(ln)      — HTTP live immediately, /healthz returns {"status":"starting"}
5. Slow init in main()      — NATS connect, store open, consumer subscribe
6. deps.Store(...)           — atomic publish, readiness gate opens for /v1/*
7. log.Fatal(<-fatal)        — block on HTTP server error channel
```

**Why `net.Listen` + `server.Serve(ln)` instead of `ListenAndServe`**: `ListenAndServe`
binds and accepts atomically — you cannot separate them. `net.Listen` in the main goroutine
guarantees the port is bound before any slow I/O begins. `server.Serve(ln)` in a goroutine
starts accepting connections on the already-bound listener.

## `atomic.Pointer[T]` for Dependency Publication

```go
type listenerDeps struct {
    client   *bus.Client
    registry *store.Registry
    sessions *session.SessionRegistry  // may be nil (graceful degradation)
}

var deps atomic.Pointer[listenerDeps]
```

**Why not a mutex**: Single writer (main goroutine stores after init), many concurrent readers
(HTTP handlers load on each request). Lock-free reads, no contention. Nil pointer is a natural
"not ready" sentinel.

**Key subtlety — two access patterns for the same dependencies:**
- **HTTP handlers**: access via `deps.Load()` — created before deps exist, gated by readiness
  middleware. Must use atomic pointer.
- **NATS consumer callback**: captures local variables directly from the init scope — created
  after all deps are initialized. Does NOT use the atomic pointer. This is correct and
  intentional; adding an atomic read on every NATS message would be unnecessary overhead.

**`sessions` can be nil even after init.** `session.OpenSessionRegistry` failure is non-fatal
(graceful degradation to file-only delivery). The atomic pointer being non-nil does not
guarantee all fields are non-nil. The `SessionRegistry` type uses nil-receiver methods that
return `ErrNoKV`, so nil dereference is safe — but callers should handle the error.

## Readiness Gate Middleware

```go
func readinessGate(ready func() bool, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !ready() {
            http.Error(w, "service starting", http.StatusServiceUnavailable)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

Applied to a **sub-mux** wrapping all `/v1/*` routes — one gate, no per-handler nil checks:
```go
v1 := http.NewServeMux()
v1.HandleFunc("/v1/interests/subscribe", ...)
mux.Handle("/v1/", readinessGate(func() bool { return deps.Load() != nil }, v1))
```

## Three-State Health Check

| Phase | `/healthz` | Body | Meaning |
|-------|-----------|------|---------|
| Starting | 200 | `{"status":"starting"}` | Alive, init in progress — don't restart |
| Healthy | 200 | `{"status":"healthy"}` | NATS connected, fully operational |
| Unhealthy | 503 | `{"status":"unhealthy","error":"..."}` | Was connected, NATS dropped |

Returning **200 during startup** is deliberate — it tells the orchestrator "I'm alive, keep
waiting" without triggering a container restart. This is bounded by the NATS connection timeout
(~10s with retries).

## Fatal Error Routing

`log.Fatal` calls `os.Exit(1)`, skipping all defers. Safe from `main()` (process is dying
anyway), dangerous from goroutines (defers in other goroutines won't run).

**Rule**: Keep fatal-error paths in the main goroutine. Background goroutines send errors to
a buffered channel:

```go
fatal := make(chan error, 1)  // buffered: goroutine won't block if main is in slow init
go func() {
    if err := server.Serve(ln); err != http.ErrServerClosed {
        fatal <- err
    }
}()
// ... slow init in main ...
log.Fatal(<-fatal)
```

The buffer size of 1 is important: if the HTTP server dies while main is still in NATS init,
the send must not block or the goroutine leaks.

## Gotchas

### Go `ServeMux` Sub-Mux Routing

When `mux.Handle("/v1/", submux)` delegates to a sub-mux, Go does **NOT** strip the `/v1/`
prefix. The inner mux must register full paths:

```go
v1.HandleFunc("/v1/interests/subscribe", ...)  // ✓ full path
v1.HandleFunc("/interests/subscribe", ...)     // ✗ won't match
```

The trailing slash on `/v1/` is required for prefix matching in `net/http`.

### `.gitignore` Bare-Name Pattern Trap

A bare pattern like `listener` in `.gitignore` matches any file or directory named `listener`
at any depth. This meant `cmd/listener/` was gitignored — new files (like `main_test.go`)
were silently invisible to `git`/`jj`.

**Fix**: Root-anchor binary names with `/listener`. For Go projects, always anchor compiled
binary ignores: `/github`, `/listener`, `/slack` — never bare names.

## Applicability to Other Receivers

The `github` and `slack` receivers (`cmd/github/main.go`, `cmd/slack/main.go`) have the same
NATS-before-HTTP pattern but are simpler (publish-only, no JetStream consumer). The same
7-phase startup, `atomic.Pointer`, and readiness gate apply directly. The `readinessGate`
function is generic (`func() bool` + `http.Handler`) and can be copied verbatim.
