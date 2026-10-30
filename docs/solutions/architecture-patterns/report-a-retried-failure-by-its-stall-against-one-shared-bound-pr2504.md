---
title: "Report a failure a retry loop owns by its stall against one shared bound"
category: architecture-patterns
tags:
  - logging
  - alarms
  - retry
  - observability
  - nats
date: 2026-10-30
status: active
module: packages/envoy
supersedes: docs/solutions/architecture-patterns/a-timer-callback-acts-only-for-the-generation-that-armed-it-INFRA-182.md
related_issues:
  - "sjawhar/legion#2504"
---

# Report a failure a retry loop owns by its stall against one shared bound

This corrects one bullet of the document it supersedes and keeps the rest. That document's rule
still holds: a timer callback acts only for the generation that armed it, read under the same
mutex that moves the generation, and a fake timer in a test keeps every callback rather than the
last. What is no longer true is its second reporter — "a path that is not a timer, and reports for
whatever is outstanding (a failed repair, say)". sjawhar/legion#2504 deleted that path
(`reportFailedRepair` in `packages/envoy/internal/kvwatch/kvwatch.go`), because a failed attempt
is not the end of a repair when something else is still repairing. One outage now has one
reporter, and the fired callback stops nothing (its own timer has already fired), so
`reportUnrepaired` takes the generation alone.

The rule:

- **When a recovery loop already retries an operation, one failed attempt is not an operator's
  business.** Report by how long the state has been uncovered: a warning under a stated bound, an
  error at or past it, with the elapsed time as an attribute on both lines.
- **Measure the stall from the first uncovered event**, not from the latest one and not from the
  attempt. A second event while the state is already stale must not restart the clock, and one
  attempt can legitimately cost tens of seconds (here each JetStream call waits a 10 s `MaxWait`,
  and the caches are rebuilt in sequence), so an attempt-based clock reports work in progress as a
  fault.
- **Emit both levels from one call site** — `slog.Log(ctx, level, msg, …)` with the level and the
  message chosen from the stall — so an attribute added later lands on both lines. Keep each
  message string stable: an alarm's log-metric filter matches one of them word by word.
- **One outage, one reporter, one bound.** When two layers each watch the same outage, share the
  bound as one exported constant rather than two equal literals, and delete the reporter that
  fires before the other layer has finished retrying.
- **Say what actually repeats.** Past the bound a recovery re-reports on every attempt (here a
  backoff capped at 30 s), so a comment claiming the outage "reports once" is false; what is true
  is that both halves report at the same minute.
- **Prove a new log-metric filter against lines the shipped binary wrote**, with the sibling line
  it must *not* count as the negative control, and move the count goldens with it. See
  docs/solutions/envoy/a-retrying-sender-multiplies-every-log-line-an-alarm-counts-so-count-a-deliverys-first-failure-and-prefix-the-repeats-pr2478.md
  for why these filters are positional and word-indexed.

## Evidence

In sjawhar/legion#2504 a KV rewatch that failed after a NATS reconnect was logged at ERROR at
once, although `internal/bus/recovery.go`'s recovery re-ran the hooks within seconds, and
`internal/kvwatch` added a second ERROR of its own for the same outage. The listener's own flaky
restart test failed on exactly that line: one attempt's deadline, repaired a moment later, read as
an outage.

The shape now (code head `4f22b8aa`): `const RepairGrace = time.Minute` and
`func (c *Client) HookStall() (time.Duration, error)` in `packages/envoy/internal/bus/nats.go`;
`markHooksDueLocked` starts the stall at the first uncovered connection event and leaves it where
it is for a later one; `reportHookFailure` in `internal/bus/recovery.go` makes one `slog.Log` call,
WARN `envoy nats reconnect hook failed; it is retried` under the bound and ERROR
`envoy nats reconnect hooks have not succeeded` at or past it, both with `name`, `error` and
`stall_ms`; and `internal/kvwatch`'s `unrepairedDelay` is `bus.RepairGrace` itself, with its own
failed-repair ERROR deleted.

The alarm `reconnect-hooks-stalled`
(`packages/envoy/infra/aws/rendering/metricfilters.go`) counts the ERROR message's level and
words. The tester matched the rendered pattern against the four ERROR lines a real listener wrote
under a NATS server that denied KV consumer creation, and against the two WARN lines it must not
count: the discriminators are the level token and `hooks` against `hook`. That is a token-by-token
application of CloudWatch's rules, not a CloudWatch run — the available proof, and no stronger.
The count goldens that had to move with the new filter are in
`packages/envoy/infra/aws/webhooklistener_test.go` and `packages/envoy/infra/render/render_test.go`.

A base control is recorded in the pull request: at `88840d35` the same scenario logs
`ERROR envoy nats reconnect hook failed` for a rewatch the retry repairs, which
`TestRewatchRetryAfterAReconnectLogsNoError` (`packages/envoy/internal/bus/hook_stall_test.go`)
now forbids.
