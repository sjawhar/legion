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
  - "#1269"
symptoms:
  - "Pulumi deploy timeout waiting for health check"
  - "health endpoint unreachable during NATS connection"
  - "503 from /healthz during startup"
  - "container appears down while connecting to NATS"
  - "new test files in cmd/listener/ not tracked by git"
  - "/v1 answers 503 \"service starting\" right after /healthz returned 200"
---

# Go Async Health Startup: Bind HTTP Before Slow Dependencies

When a Go service depends on slow external resources (NATS, databases, remote APIs), the
health check endpoint must be reachable before those dependencies initialize. Otherwise
orchestrators (Pulumi, Kubernetes) time out and kill the container.

## The Pattern: 7-Phase Startup

```
1. config.Load()           — synchronous, fast
2. net.Listen("tcp", addr) — bind port in main goroutine (deterministic)
3. Build HTTP mux           — /healthz always available, /v1/* and webhooks behind a gate
4. go server.Serve(ln)      — HTTP live immediately, /healthz returns {"status":"starting"}
5. Slow init in main()      — NATS connect, store open, consumer subscribe
6. deps.Store(...)           — atomic publish; handlers built, gates open for /v1/* and webhooks
7. log.Fatal(<-fatal)        — block on HTTP server error channel
```

**Why `net.Listen` + `server.Serve(ln)` instead of `ListenAndServe`**: `ListenAndServe`
binds and accepts atomically — you cannot separate them. `net.Listen` in the main goroutine
guarantees the port is bound before any slow I/O begins. `server.Serve(ln)` in a goroutine
starts accepting connections on the already-bound listener.

## Build Handlers Once, From Complete Dependencies

```go
type listenerDeps struct {
    client   *bus.Client
    registry *store.Registry
    sessions *session.SessionRegistry
    ciStore  *cistore.Store
    // ...
}
```

The listener exits when the interest registry, the session registry or the CI store cannot
open, so a `listenerDeps` exists only with every store open. The webhook and `/v1` handlers take
it as a plain `*listenerDeps` and are built once, after it is complete: no handler loads a
pointer on each request, and none can be constructed over a dependency that is not there.

**Only what answers during startup reads an atomic pointer.** `/healthz` and the metrics
gauges run before NATS is up, so they read `deps atomic.Pointer[listenerDeps]`, nil until
phase 6, which is their "starting" sentinel. The NATS consumer callback captures the
initialized locals directly.

## The Starting Gate

```go
type startingGate struct {
    handler atomic.Pointer[http.Handler]
}

func (g *startingGate) open(handler http.Handler) { g.handler.Store(&handler) }

func (g *startingGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    handler := g.handler.Load()
    if handler == nil {
        writeJSONError(w, http.StatusServiceUnavailable, "service starting")
        return
    }
    (*handler).ServeHTTP(w, r)
}
```

main registers the `/v1` paths on one gate and every enabled webhook path on another before
`server.Serve`, so those paths answer 503 while the listener starts. In phase 6 it builds each
family's mux over the complete deps and opens its gate onto it:

```go
var webhooks, v1 startingGate
mux.Handle("/v1/", apiAuth(apiToken, apiVerifier, logger, &v1))
// ... phase 6, once every store is open:
v1Mux := http.NewServeMux()
registerV1Routes(v1Mux, ready, cfg.MachineID, logger)
v1.open(v1Mux)
```

## Health States

| Phase | `/healthz` | Body | Meaning |
|-------|-----------|------|---------|
| Starting | 200 | `{"status":"starting"}` | Alive, init in progress — don't restart; `/v1/*` answers 503 |
| Healthy | 200 | `{"status":"healthy"}` | NATS connected, fully operational; `/v1/*` is open |
| Degraded | 200 | `{"status":"degraded","error":"..."}` | A KV dependency or the durable-consumer lookup failed transiently; NATS reconnect and the monitor retry |
| Unhealthy | 503 | `{"status":"unhealthy","error":"..."}` | NATS, the subscription, a KV watcher or the durable consumer is gone |

Returning **200 during startup** is deliberate — it tells the orchestrator "I'm alive, keep
waiting" without triggering a container restart. Startup is not short. The NATS connect makes up to
10 attempts with a 5 s timeout each, 1 s apart (`internal/bus/nats.go`), so an unreachable NATS
keeps the listener `starting` for about a minute before `log.Fatal`. The interest and session
cache warm-ups are bounded at 30 s each. The durable-consumer subscribe tries up to 10 times
with a 3 s × attempt backoff, 135 s of sleeps in all (`cmd/listener/main.go`), so a rolling
deploy that waits for the old task's binding can stay `starting` for minutes.

Because a starting listener answers 200, a start that cannot succeed must end rather than wait:
a deploy that trusts the 200 would stop the old task for a replacement that never serves. A
durable the listener refuses (an idle heartbeat or an ack policy NATS cannot change in place) is
the one subscribe failure no retry can fix, so the listener checks its durable right after the NATS
connect, before the cache warm-ups, and on a refusal exits 1 at once instead of retrying.

**A 200 is liveness, not readiness.** Anything that calls `/v1` after starting the listener — a
test harness, an e2e script — waits for the `"healthy"` body, or for a 200 from a `/v1` route,
never for a bare 200 from `/healthz`. The container smoke waited for a bare 200 and flaked on a
slow runner: its `/v1` subtests ran inside the 503 window (sjawhar/legion#1269).

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
7-phase startup and the starting gate apply directly; `startingGate` holds nothing
listener-specific and can be copied verbatim.
