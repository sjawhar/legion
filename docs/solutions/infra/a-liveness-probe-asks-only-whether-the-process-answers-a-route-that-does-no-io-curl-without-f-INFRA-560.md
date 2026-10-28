---
title: "A liveness or startup probe asks only whether the process answers — a route that does no I/O, curl without -f, and readiness keeps the dependency checks"
category: infra
tags:
  - kubernetes
  - liveness
  - readiness
  - probes
date: 2026-10-27
status: active
module: packages/envoy/infra/gcp
related_issues:
  - "INFRA-560"
  - "sjawhar/legion#2368"
---

# A liveness or startup probe asks only whether the process answers

- Point liveness and startup probes at a route that reads nothing (`GET /livez`, answering 204).
  A `/healthz` that checks Postgres, NATS or a bucket can be slow or fail while a dependency is
  down, and a liveness probe on it turns one dependency outage into a restart of every pod, which
  fixes nothing.
- Run `curl -sS -o /dev/null --max-time <t>` without `-f`, with `--max-time` below the probe's
  `timeoutSeconds`: any HTTP answer, a 404 or a 401 included, means the process answers. A process
  with no such route still passes, since its router answers without I/O.
- Keep `/healthz` with `curl -fsS` for readiness only, so a pod whose dependency is down leaves
  its Service and stays running.
- Give the startup probe the longest time the process may take before it listens (a migration
  that runs before `listen` has no bound), so liveness never sees that window.

## Evidence

- sjawhar/legion#2368's plan probed `/healthz` with curl and no `-f`. The simplify pass at
  `b2c39930` moved liveness and startup to a new `/livez`
  (`packages/envoy/cmd/dispatch/main.go`, `dispatchHandler`) because the listener's `/healthz` can
  take longer than curl's 8 s on a slow NATS hub, and three failures in a row would restart the
  listener.
- `answersProbe` and `readyProbe` are in `packages/envoy/infra/gcp/dispatch_kube.go`; the app's
  startup probe allows 30 minutes (180 tries 10 s apart) because `envoy-dispatch` migrates before it
  listens.
