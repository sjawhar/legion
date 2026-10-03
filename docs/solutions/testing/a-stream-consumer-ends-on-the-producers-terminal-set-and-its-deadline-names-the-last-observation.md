---
title: "A test that consumes a verdict stream ends on the producer's terminal set, tolerates what the producer keeps on watch, and names the last observation at its deadline"
category: testing
tags:
  - flaky-tests
  - go-test
  - tmux-runtime
  - observation-stream
  - deadline-failures
  - non-verbose-ci
  - daemon-go
date: 2026-10-02
status: active
module: packages/daemon-go/internal/runtime/tmux
applies_when:
  - A test drains `runtime.Observe` (or any channel of verdicts) and waits for a process to be reported dead
  - A wait loop copies its accept/fail predicate from a precedent helper that polls `Probe` directly
  - A deadline `t.Fatal` in a test CI runs without `-v` carries no value
  - A real-tmux or sandbox live test is reported flaky on a verdict it did not expect
related_issues:
  - "LEGION-370"
  - "sjawhar/legion#1678"
  - "LEGION-274"
---

# A Test That Consumes a Verdict Stream Ends on the Producer's Terminal Set

- Before writing a loop that waits on a channel of verdicts, read the producer's removal rule and
  end the loop on exactly that set. Tolerate every kind the producer keeps on watch; the producer
  re-probes those itself, and a loop that fatals on one asserts a distinction the system never
  makes. Bound the tolerance with a deadline.
- Then read the production consumer of the same stream. Never fail on a verdict the consumer
  treats as the one you wanted.
- A precedent helper lends you its derivation, not its predicate. Re-derive the accept and fail
  sets from what the test established before the wait and from who produces the stream: a
  direct `Probe` poll keeps probing after a transient verdict; an untracking sweep stops.
- Every deadline `t.Fatal` on a stream prints the last item received. CI runs `go test` without
  `-v`, so that one line is the whole triage artifact; keep its leading substring stable when the
  proofs quote it.

## The rule applied to the tmux runtime

The sweep (`observe.go:40-63`) probes every tracked process each `ProbeInterval` and untracks
only on `Gone` or `NotRecordedProcess` (`observe.go:58-60`); `Alive` and `Uncertain` stay tracked
and are probed again. The supervisor takes `Gone` and `NotRecordedProcess` as the same death
(`internal/supervise/machine.go:806`) and keeps `Uncertain` alive on a streak counter
(`machine.go:808-812`). So for a test that drains `Observe` after killing a pane, the terminal set
is `{Gone, NotRecordedProcess}`, the tolerated set is `{Alive, Uncertain}`, and nothing else can
arrive from `Probe` (`verify.go:295-307`).

`awaitGone` (`tmux_real_test.go:604-624`) returns only on `Gone` and fatals on `Alive`. Both are
right for it: it runs after a `stop` that already returned, so `Alive` is a contract violation,
and it polls `Probe` directly, where a `NotRecordedProcess` is transient and `Gone` does arrive.
Copying its predicate into a sweep-consuming loop after an external `kill-pane` reproduces the
flake in a new shape: after `NotRecordedProcess` the sweep sends nothing more for that locator,
so a loop still waiting for `Gone` can only hit its deadline.

The same untrack rule is `internal/runtime/sandbox/watch.go:130-131`, and its live rig is split:
`live_secrets_test.go:398` accepts either final verdict, `live_lifecycle_test.go:441-442` awaits
`Gone` alone against the same producer. Re-derive the second from the producer before trusting it
under load.

## Evidence (LEGION-370)

`TestRealTmuxObserveReportsGoneOnce` (`tmux_real_test.go:1287`) fataled on any post-kill kind that
was neither `Alive` nor `Gone`. Under the load recipe of
`widen-the-contender-count-before-calling-a-race-unreproducible.md` (8 `nice -n 10` busy loops +
one fsync writer), the unchanged test failed 12 of 40 runs in the implementer's pod (13 of 40 for
the planner, 10 and 9 of 40 for the tester): 7 `Kind:uncertain Detail:cannot verify pane %N:
list-panes -t %N exited 1: no current target` (the server's last session destroyed while the
server has not yet exited; `no current target` does not match `paneGoneStderr`, `verify.go:20-25`),
and 5 `Kind:not_recorded_process` (`has no readable /proc stat`, `is not running OMP`: a probe
whose `list-panes` listed the recorded pid and whose `/proc` reads ran after the death,
`verify.go:156-171`). The second kind contradicted the spec's first state model ("fail only on
`NotRecordedProcess`"); the measurement went to the architect and the spec moved to v3 before any
code was written.

The loop now ends on either final verdict, tolerates `Alive` and `Uncertain`, keeps the 10 s
deadline and the 1 s "never again" check, and keeps the last observation for the deadline fatal
(`tmux_real_test.go:1313-1328`). After the change: 200 of 200 idle and 200 of 200 loaded in each of
two pods; CI's `daemon-go` job green at `fec3927b`, `aa02c6d2`, `f6739738`.

The deadline fatal first said only `the sweep never reported the killed pane gone`. Review round 1
(`sjawhar/legion#1678`, thread `r4168110448`) pointed out that a classifier stuck on `Uncertain`,
one stuck on `Alive`, and a sweep gone silent would all print that same line, and that the old
per-observation fatal's `Kind:uncertain Detail:… no current target` was how the issue had been
diagnosed at all. With `var last runtime.Observation` assigned in the receive arm and printed as
`…; last = %+v`, the three faults print three different lines (tester, 5/5 or 2/2 each): a gone
pane read as alive prints `Kind:alive … Detail:pane %1 is gone`; every failed listing read as
`Uncertain` prints `Kind:uncertain … no server running on …`; a sweep with `ProbeInterval` set to
an hour prints the zero `Observation`. `tmux_real_test.go:1280` (`the restarted daemon's sweep
reported nothing`) is a single first-observation wait, so "nothing" is accurate there; a loop is
not.

## Related

- `docs/solutions/testing/mutation-proof-probe-tests.md` — the negative controls for this change:
  a control for a widened accept set flips every accepting path at once.
- `docs/solutions/testing/widen-the-contender-count-before-calling-a-race-unreproducible.md` —
  the load recipe, and the failure census that checked the spec's state model.
- `docs/solutions/testing/a-worker-pod-ships-no-go-or-tmux-provision-them-under-home-each-relaunch-and-record-every-figure-in-the-handoff.md`
  — how the proof above was run from worker pods.
- `docs/solutions/testing/await-the-event-not-a-tick-budget.md` — await the production event, not
  a tick budget; this document is about which event, when the stream has several.
