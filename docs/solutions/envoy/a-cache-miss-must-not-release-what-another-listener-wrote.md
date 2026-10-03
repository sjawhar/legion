---
title: "A listener's cache miss must not release what another listener just wrote"
category: envoy
tags:
  - nats
  - jetstream
  - kv
  - cache
  - flaky-test
  - role-lanes
date: 2026-10-03
status: active
module: envoy
related_issues:
  - "LEGION-456"
symptoms:
  - "TestARoleMessageReachesItsHolderOnceAcrossTwoTasksOfOneMachine times out at startup_test.go:177 waiting for the old task to see the holder"
  - "GET /v1/roles/<role> answers holder_lapsed, claim released, for a holder that registered a moment ago through another listener"
  - "a role becomes unclaimed fleet-wide right after a claim, and its messages report delivery_failed then no_holder"
---

# A Listener's Cache Miss Must Not Release What Another Listener Just Wrote

Each listener follows the interest, session and CI buckets through a watcher-fed cache
(`internal/kvwatch`), so another listener's write reaches this listener's cache only when the
watcher delivers it. The role bucket has no cache: a claim is read straight from it. A decision
that reads one bucket directly and another through a cache can see a fresh write in the first and
not yet in the second.

Role resolution did exactly that. A holder registers and claims through listener B: the session
`Put`, then the claim `Create`. Listener A then resolves the role. It reads the claim from the
bucket, misses the holder in its session cache, and releases the claim as lapsed
(`ReleaseExpiredRoleClaim`). The claim is gone for every listener, B included, until the holder
claims again. During a rolling deploy, A is the old task and B the replacement, and both serve the
machine's role lane.

## Rules

- **A cache miss may refuse or retry. It may not destroy state another listener wrote.** Before
  releasing, deleting or superseding on a session's absence, read the session from the bucket
  (`session.SessionRegistry.Refresh`, through `roleHolderSession` in `cmd/listener/api.go`). A read
  that does not answer takes nothing. A plain `Get` stays right for a send to a missing target,
  which refuses and lets its caller retry.
- **Don't fix it by making `SessionRegistry.Get` fall back to the bucket.** Fan-out checks the
  session of every matching interest on every message (`fanoutDelivery`, `cmd/listener/delivery.go`).
  A miss there is usually a dead session whose interest the reaper has not removed yet, so a
  fallback would cost one KV round trip per such interest per message. Read the bucket only where a
  miss would destroy something.
- **A test that polls for a state the code destroys hides why.** The e2e test polled
  `GET /v1/roles/<role>` for 30 s and threw away each answer. The first answer said
  `claim released`; the next 557 said `unclaimed`. Assert the first answer and print it.

## Reproducing a cross-process cache-lag race

The natural window is a few milliseconds. On the devbox the e2e test failed once in 800 runs:
0/200 plain, 0/200 `-race`, 0/200 on two CPUs shared with busy loops, and 1/200 in a scope throttled
to half a CPU. To hold the window open without touching the code under test, slow the watcher
inside the listener binary the test builds:

- `buildListener` (`cmd/listener/shutdown_test.go`) passes `-overlay $ENVOY_TEST_GO_OVERLAY` to
  `go build`. Point it at a JSON `Replace` map from `internal/session/registry.go` to a copy that
  sleeps at the top of `applyWatched`.
- `startListenerProcess` gives the listener only the environment the test lists, so an environment
  variable never reaches the process. Hard-code the delay in the copy, or read it from a file.

With a 20 ms delay, main failed 180 of 200 runs, every one at `startup_test.go:177`. The fix failed
0 of 200. A log line added through the same overlay to `roleGetHandler` showed the mechanism directly:
`state=holder_lapsed ... claim_release=0` on the first lookup, then `state=unclaimed`. When the
gap between steps is a shell spawning curl, it already outlasts 20 ms; driving two listener
binaries by hand needed 500 ms to reproduce.
