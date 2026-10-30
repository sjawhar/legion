---
title: "One function decides whether a view answers, and a self-repairing dependency is reported last"
category: envoy
tags:
  - health-checks
  - readiness
  - listener
  - kvwatch
date: 2026-10-30
status: active
module: packages/envoy/cmd/listener
related_issues:
  - "INFRA-757"
  - "sjawhar/legion#2517"
---

# One function decides whether a view answers, and a self-repairing dependency is reported last

Extends `docs/solutions/envoy/async-health-startup-pattern.md`, which has the
`200 degraded` / `503 unhealthy` ladder itself. This adds who decides, where in the handler the
decision goes, and what the answer must carry.

**One function decides whether a view can answer, and the gate, the route and the health report
all call it.** A cache with a weaker readiness flag and a stronger answerability condition has two
predicates, and a gate on the weaker one serves a view that refuses every read. The rule is not
"never gate on readiness": a view that answers from a partial cache is rightly gated on readiness,
and a view that refuses until its scan is complete is not. Each view's gate is that view's own
answerability condition, and exactly one function decides it, so what a route refuses and what a
probe reports cannot drift apart. (The same rule for daemon holds is
`docs/solutions/daemon/a-hold-is-computed-with-the-check-the-action-that-releases-it-uses-INFRA-252.md`.)

**Report a self-repairing dependency last, after every check that answers the terminal status.**
A readiness probe passes on any 2xx, so a `degraded` answer placed before a terminal check hides
that terminal fault behind a 200 and leaves a listener that receives nothing in service. Put the
self-repairing check after every check that can answer 503, and keep the terminal checks in the
order they already had.

**Build the degraded answer on the healthy body.** A reader of the counters loses none of them
while the dependency repairs itself, and one helper produces the `error` text for both the
degraded-with-fields answer and the terminal one, so the two cannot drift.

**Keep a self-repairing dependency out of the health call that feeds a self-health monitor.** A
monitor that restarts the process over a terminal failure surviving its recovery attempts will
restart it forever over a fault no restart repairs; what repairs that one is the watcher's own
rescan, which needs nothing from the monitor.

**Where two causes share one refusal shape, every assertion on either side pins its cause.** A
refusal whose status, header and message prefix are deliberately the same for two causes — because
clients match the prefix — is an ambiguity you designed in. A test that asserts only the shape
passes for the other cause: require the distinguishing reason where the test is about one cause,
and forbid it where the test is about the other.

**Cross the product and the tests to decide where a flake's fix belongs.** Run base product with
base tests, base product with the branch's tests, and the branch with both, under the forcing
device. If the branch's tests still fail on the base product, no test-side wait can fix it and the
fix belongs in the product.

## Evidence (INFRA-757, sjawhar/legion#2517)

`kvwatch.Watcher.Ready` is released by a first scan nats.go's idle timer ended;
`store.Registry.RolesHeldBy` answers only while that watcher is `Complete`. The listener's startup
gate waited on readiness, so `GET /v1/sessions` and `GET /v1/registry/<session_id>` answered
`envoy: the message bus did not answer` on a healthy bus, which is how the chance failure reached
CI. `roleViewGeneration` is now the one state test, read by `RolesHeldBy` and by `RoleViewErr`.

The first `/healthz` fix put the view's `degraded` answer before the durable consumer's info read
and its bound check, both of which answer 503: with the view incomplete, a deleted durable
answered `200 degraded` and the body lost `session_cache_*`, `num_pending` and `num_ack_pending`.
`TestHealthzStillReportsALostDurableWhileTheRoleClaimViewIsIncomplete` is the crossed pair —
self-repairing fault plus terminal fault, assert the terminal one — and it fails against the
previous handler with `= 200 map[... status:degraded], want 503 unhealthy`.

One hazard this change did not close, and it is older than the change: a transient KV ping failure
and a durable info read that did not answer each write their own `200 degraded` and return, ahead
of the terminal checks below them. The ordering rule above applies to them too.

The crossing that moved flake B's fix into the product, each row under the same forcing device
(the role claim bucket's first watch ending its scan as the idle timer does): base product with
base tests failed; base product with the branch's tests failed the same way, which is the row that
decided it; the branch passed; and with the device off, the base passed, so the loop under load
proved nothing on its own.
