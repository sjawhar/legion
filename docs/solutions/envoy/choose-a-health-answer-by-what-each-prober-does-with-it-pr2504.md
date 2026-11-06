---
title: "Choose a health answer by what each prober does with it, and decide a new state last"
category: envoy
tags:
  - health-check
  - readiness
  - observability
  - alarms
  - nats
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2504"
---

# Choose a health answer by what each prober does with it, and decide a new state last

Extends docs/solutions/envoy/async-health-startup-pattern.md.

That document says what the listener's `/healthz` answers and that a container health check acts
on it. This one is the rule for **adding** a state to such an endpoint.

- Before you add a state, list every prober that reads the endpoint **and the replica count of
  what it probes**. For the Envoy listener that is six: the ECS container health check
  (`packages/envoy/infra/aws/rendering/listener.go`, `curl -sf`) on a one-task service
  (`infra/aws/webhooklistener.go`), GKE readiness (`infra/gcp/dispatch_kube.go`, `readyProbe`),
  the image's own `HEALTHCHECK` (`packages/envoy/docker/Dockerfile`), two compose health checks
  (`packages/envoy/deploy/compose/`), and the on-prem watchdog
  (`packages/envoy/infra/host/watchdog.go`).
- **Refuse only when a replacement can do better than the process you are refusing for.** When the
  fault is in the process's own rebuild path, a refusal replaces or unroutes the only process
  there is, and its replacement re-enters the same failing rebuild with cold state — worse than
  the stale state the running one still serves. Answer a non-refusing state naming the dependency,
  and move the paging signal out of band, to a log-metric alarm on the error line the process
  already writes.
- **Decide a new, less severe state last**, below every refusing check, and build the healthy
  response body only after every check. A state that returns early reports its own fault while a
  worse one is true, to every prober at once.
- **Pin that order with a test that puts a worse fault under the new state**, not with a comment.
  The test must fail when the new check is moved back above the refusing checks.
- **Keep the policy where the answer is decided.** The library exposes the fact — a duration and
  the last error — and the handler decides the status word; share only the bound, as one exported
  constant. A library that returned "the formatted error, but only past the bound" would move the
  health policy out of the handler that owns it.

The counter-case is in the same census: a deployment that runs two or more of the process
(`packages/envoy/infra/gcp/clusteredhub_tailnet.go`) can be refused for, because a replacement is
not the last one serving. The rule is about the single-replica case and about faults the
replacement re-enters, not "never answer 503".

## Evidence

sjawhar/legion#2504 (code head `4f22b8aa`) put a stalled KV watch rebuild on the listener's
`/healthz`. The plan chose `503 unhealthy`, on the premise that no probe acts on `/healthz`.

Review round 1 showed the premise false and the blast radius concrete: the ECS check is
`curl -sf …/healthz` every 30 s with 3 retries against a service of one task, so about two minutes
into a stall ECS stops the only listener; the replacement's own first watch needs the same KV
consumer creation that is failing, so its caches warm up empty, it binds the durable (which
already exists) and its fan-out then acknowledges every general-topic message as
`listener no matching interests` (`cmd/listener/delivery.go`) — messages lost, against the
issue's "no change to delivery semantics". The running task, stale but not empty, was still
delivering. The answer became `200 degraded` naming the dependency, and the paging signal became
a new log-metric alarm, `reconnect-hooks-stalled`, on the ERROR the bus writes
(`packages/envoy/infra/aws/rendering/metricfilters.go`).

Review round 2 found the second half: the new branch returned before the durable-consumer checks,
so while a stall was past its bound a durable that was gone or unbound answered `200 degraded` and
named the wrong fault — on a listener that then receives nothing at all. The branch moved below
every 503 check, and `TestHealthzReportsARecreatedDurableThroughAHookStall`
(`packages/envoy/cmd/listener/healthz_hook_stall_test.go`) holds it: a stall past the bound with a
recreated durable must answer 503. Moving the check back above the durable checks fails that test
with `200 … degraded, want 503 unhealthy`. Round 3 moved the healthy response map below the checks
that discard it.

The reviewer also offered to move the comparison into the bus as a `HookStallErr()`; it was
declined on the thread, because with the answer now a 200 the handler still decides the status
word, and only the bound (`bus.RepairGrace`) is shared.
