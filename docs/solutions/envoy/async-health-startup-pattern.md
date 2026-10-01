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
3. Build HTTP mux           — /healthz always available, /v1/* and webhooks each behind a gate
4. go server.Serve(ln)      — HTTP live immediately, /healthz returns {"status":"starting"}
5. Slow init in main()      — NATS connect, CI store open, the webhook gate opens, then the
                              other stores open and their caches warm
6. role lane, v1Gate.open,  — the role lane subscribes, the /v1 gate opens onto the stores, the
   bind, deps.Store            durable binds (polled every 2 s for up to 135 s), then the
                              atomic publish turns /healthz healthy
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
open, so a `listenerDeps` exists only with every store open. The `/v1` handlers take it as a
plain `*listenerDeps`, and the webhook handlers take the only two things they use, the NATS
client and the CI store; each is built once, after what it takes is open: no handler loads a
pointer on each request, and none can be constructed over a dependency that is not there.

**Only what answers during startup reads an atomic pointer.** `/healthz` and the metrics
gauges run before NATS is up, so they read `deps atomic.Pointer[listenerDeps]`, nil until the
durable binds at the end of phase 6, which is their "starting" sentinel. The NATS consumer
callback captures the initialized locals directly.

## The Starting Gate

```go
type startingGate struct {
    name   string
    logger *logging.Logger
    mux    atomic.Pointer[http.ServeMux]
}

func (g *startingGate) open(mux *http.ServeMux) { g.mux.Store(mux) }

func (g *startingGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    mux := g.mux.Load()
    if mux == nil {
        g.logger.Warn("request refused while starting", /* gate, method, path */)
        writeJSONError(w, http.StatusServiceUnavailable, "service starting")
        return
    }
    mux.ServeHTTP(w, r)
}
```

main registers two gates before `server.Serve`, one bare on every enabled webhook path and one
inside `apiAuth` on `/v1`, so those paths answer 503 while the listener starts, and each refusal
logs `request refused while starting` with the gate, the method and the path: that line is the
listener's only record of a caller it turned away. Once NATS and the CI store are open,
`openWebhooks` builds the webhook handlers and opens the webhook gate onto them. In phase 6, once
the interest and session caches are warm, the role lane subscribes and `openV1Routes` builds the
`/v1` routes over the stores and opens the `/v1` gate onto them, logging `envoy-listener /v1 open`
with `since_listening_ms`. Neither gate waits for the durable consumer's bind: no webhook and no
`/v1` handler reads the durable, and during a rolling deploy the bind waits out the task being
replaced while the load balancer already sends the replacement webhooks and callers that resolve
the listener's name already reach it. GitHub does not redeliver a refused delivery, and a refused
`/v1` call is a failed Dispatch delivery or a lost publish. The role lane opening early forwards no
role message twice: both tasks of a machine are one core-NATS queue group, and NATS hands each
message to one member.

```go
webhookGate, v1Gate := newStartingGate("webhook", logger), newStartingGate("v1", logger)
for _, hook := range hooks {
    mux.Handle(hook.path, webhookGate)
}
mux.Handle("/v1/", apiAuth(apiToken, apiVerifier, logger, v1Gate))
// ... phase 5, once NATS and the CI store are open:
openWebhooks(webhookGate, hooks, client, ciStore)
// ... phase 6, once the interest and session caches are warm:
openV1Routes(v1Gate, ready, cfg.MachineID, logger, listeningAt)
bind, err := bindListenerDurable(stopping, client, consumer, handler, logger, durableBindInterval, durableBindDeadline)
// ... then deps.Store(ready)
```

`TestTheV1APIIsServedWhileAnotherTaskHoldsTheDurable` runs the listener binary against a durable
another subscriber holds and requires a webhook, `/v1` and the role lane to be served while
`/healthz` still says `starting`, until the holder lets go and the listener turns healthy.
`TestARoleMessageReachesItsHolderOnceAcrossTwoTasksOfOneMachine` runs two listeners of one machine
id and requires each of 64 role messages to reach its holder once, split across both tasks.

**Publish `deps` last.** Storing `deps` is what turns `/healthz` healthy, so it comes after both
gates open and after the durable binds: `/healthz` is healthy only when every route serves and the
durable delivers. Healthy before the bind would let a replacement that cannot bind read as ready,
and ECS would stop the task still delivering; a 503 instead of `starting` would never let ECS stop
the old task, so the replacement could never bind.

## Health States

| Phase | `/healthz` | Body | Meaning |
|-------|-----------|------|---------|
| Starting | 200 | `{"status":"starting"}` | Alive, init in progress — don't restart; webhooks answer 503 until NATS and the CI store are open, `/v1/*` until the interest and session caches are warm too; both are served while the durable consumer binds, which `starting` alone waits for |
| Healthy | 200 | `{"status":"healthy"}` | NATS connected, fully operational; `/v1/*` and webhooks are open |
| Degraded | 200 | `{"status":"degraded","error":"..."}` | A KV dependency or the durable-consumer lookup failed transiently; NATS reconnect and the monitor retry |
| Unhealthy | 503 | `{"status":"unhealthy","error":"..."}` | NATS, the subscription, a KV watcher or the durable consumer is gone |

Returning **200 during startup** is deliberate — it tells the orchestrator "I'm alive, keep
waiting" without triggering a container restart. Startup is not short. The NATS connect makes up to
10 attempts with a 5 s timeout each, 1 s apart (`internal/bus/nats.go`), so an unreachable NATS
keeps the listener `starting` for about a minute before `log.Fatal`. The interest and session
cache warm-ups are bounded at 30 s each. The durable's bind is polled every 2 s for up to 135 s
(`bindListenerDurable`, `cmd/listener/main.go`), so a rolling deploy that waits for the old task's
binding stays `starting` until the old task stops, while `/v1` and the webhooks already serve.

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

