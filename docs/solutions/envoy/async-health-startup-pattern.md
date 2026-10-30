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
  - "#2404"
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
4. go server.Serve(ln)      — HTTP live immediately, /healthz answers 503 {"status":"starting"}
5. Slow init in main()      — NATS connect (retried every 2 s until NATS answers), CI store open,
                              the webhook gate opens, then the listener state bucket and the other
                              stores open and their caches warm
6. role lane, v1Gate.open,  — the role lane subscribes, the /v1 gate opens onto the stores, the
   lease and receive loop      delivery lease and the durable's receive loop start, then the
   start, deps.Store           atomic publish turns /healthz 200, as the active pod or a standby
7. select on fatal/signal   — block on HTTP server error channel or SIGTERM, then shut down
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

The listener does not serve until the interest registry, the session registry and, with the GitHub
route, the CI store are open: an open that fails for want of NATS is retried every 2 s, and one the
NATS user's grant refuses ends the start with an ERROR naming the bucket, so a `listenerDeps`
exists only with every store open. The `/v1` handlers take it as a plain `*listenerDeps`, and the
webhook handlers take the only two things they use, the NATS client and the CI store; each is
built once, after what it takes is open: no handler loads a pointer on each request, and none can
be constructed over a dependency that is not there.

**Only what answers during startup reads an atomic pointer.** `/healthz` and the metrics
gauges run before NATS is up, so they read `deps atomic.Pointer[listenerDeps]`, nil until main
publishes it at the end of phase 6, once the receive loop has started, which is their "starting"
sentinel. The receive loop delivers through the initialized locals directly.

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
with `since_listening_ms`. Neither gate waits for the durable consumer: no webhook and no `/v1`
handler reads the durable, and a machine id runs an active pod and standbys
(`packages/envoy/cmd/listener/README.md`), every one of which serves the webhooks and `/v1` while
only the holder of the machine's delivery lease receives the durable. GitHub does not redeliver a
refused delivery, and a refused `/v1` call is a failed Dispatch delivery or a lost publish. The role
lane opening on every pod forwards no role message twice: every pod of a machine is in one
core-NATS queue group, and NATS hands each message to one member.

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
deliveryLease.Start(leaseCtx)
receiver.Start()
deps.Store(ready)
```

`TestTheV1APIIsServedWhileAPreLeaseBuildHoldsTheDurable` runs the listener binary against a
durable another subscriber holds without the lease and requires a webhook, `/v1` and the role lane
to be served, with `/healthz` 200 as a standby waiting for that binding, until the holder lets go
and the listener turns active. `TestARoleMessageReachesItsHolderOnceAcrossTwoTasksOfOneMachine`
runs two listeners of one machine id and requires each of 64 role messages to reach its holder
once, split across both.

**Publish `deps` last.** Storing `deps` is what turns `/healthz` from `503 starting` to `200`, so it
comes after both gates open and after the receive loop starts: `/healthz` is 200 only when every
route serves. It does not wait for the lease or the durable: a standby that serves is ready, and a
readiness probe must send it traffic, or a rolling deploy could never make the replacement ready
while the old pod still delivers.

## Health States

| Phase | `/healthz` | Body | Meaning |
|-------|-----------|------|---------|
| Starting | 503 | `{"status":"starting"}` | Up, init in progress: NATS connecting, caches warming or the OIDC issuer's discovery pending. Webhooks answer 503 until NATS and the CI store are open, `/v1/*` until the interest and session caches are warm too |
| Active | 200 | `{"status":"healthy","role":"active",...}` | Serving, and this pod receives the durable; the body carries its lag |
| Standby | 200 | `{"status":"healthy","role":"standby","active_instance":...}` | Serving; another pod of the machine id holds the lease |
| Degraded | 200 | `{"status":"degraded","error":"..."}` | A KV dependency or the durable's lag read failed transiently (NATS reconnect and the monitor retry), or this pod is the machine's deliverer and does not receive the durable yet: it holds the delivery lease, runs without one, has read no holder at all, or the lease record still names it while its renewals have lapsed. Nothing of the durable reaches a session until it receives, so no such answer is healthy; it stays 200, so a readiness probe keeps the pod in service |
| Unhealthy | 503 | `{"status":"unhealthy","error":"..."}` | NATS, the role subscription, a KV watcher or, on the active pod, the subscription or the durable consumer is gone |

Every answer comes within 5 s (`healthzDeadline`): a check still running then makes the answer
503, so a readiness probe with a longer timeout always gets one.

Returning **503 during startup** is deliberate since #2404: `/healthz` is the readiness of
every pod of a machine id, and a pod that answered 200 before it served would be sent traffic it
refuses. Startup is not short, and it never gives up on NATS: the connect is retried every 2 s for
as long as NATS does not answer, so a listener started before its NATS stays up and `starting`
rather than exiting. The interest and session cache warm-ups are bounded at 30 s each. A liveness
probe therefore must not fail on a 503: it checks only that the process answers (curl without
`-f`, with a time limit above the 5 s deadline), and the readiness probe holds traffic back.

A start that cannot succeed still ends rather than waits: a durable the listener refuses (an idle
heartbeat or an ack policy NATS cannot change in place) is the one durable failure no retry can
fix, so the listener checks its durable right after the NATS connect, before the cache warm-ups,
and on a refusal exits 1 at once; so does a store bucket the NATS user's grant refuses.

**A 200 means the pod serves.** Before #2404 the listener answered 200 `starting` from the
moment its port was bound, and anything that called `/v1` after starting it had to wait for the
`"healthy"` body: the container smoke waited for a bare 200 and flaked on a slow runner, its `/v1`
subtests running inside the 503 window (sjawhar/legion#1269). Waiting for `"healthy"` is still the
check that also says which role the pod took.

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

