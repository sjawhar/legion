---
title: "A fake-CLI suite beside live daemons asserts over each command's own calls by session, and reaches a late bound with a fault at that exact call"
category: testing
tags:
  - bash
  - fake-cli
  - flaky-test
  - call-log
  - negative-control
date: 2026-11-03
status: active
module: packages/envoy/loadtest/cluster
related_issues:
  - "sjawhar/legion#2462"
---

# A fake-CLI suite beside live daemons asserts over each command's own calls by session, and reaches a late bound with a fault at that exact call

Extends `docs/solutions/testing/fake-cli-on-path-outputs-from-files-and-a-call-log.md`.
That note makes the fake's call log the assertion surface.

- **Tag each call with the caller's session when daemons write the same log.**
  The fake writes a second log with one line per call, prefixed with the session id it runs in (field 6 of its own `/proc/$$/stat`, which every process a command starts inherits, while each daemon started under `setsid` has its own).
  Each assertion reads only the calls of the command under test.
  Never judge a command by a `wc -l` mark or by "the line before". A heartbeat or sampler that appends during the command breaks both.
  One green run proves nothing here. Run the suite several times in a row under a CPU loader (four busy loops on two CPUs) before you call it fixed.

- **To test a bound that is reached only after a step succeeded, make the fake fail at exactly that step.**
  A fault mode that cuts the target off earlier ends the case on an earlier bound.
  One that answers at once after the step never reaches the bound at all.
  For "no exit status within one grace of a delivered SIGKILL", let the KILL through and then refuse every later exec.
  Show the case is red under two mutations of the bound: the timer not set, and the branch removed.

- **Wrap each case that can hang in a watchdog,** so a lost signal fails the case instead of the suite timing out.

## Evidence

sjawhar/legion#2462, `packages/envoy/loadtest/cluster/cluster.test.sh`: the tagged log (line 208), `guarded_by_lock_read` (line 971), `own_calls_since` (line 990).
- Tester round 11 found two reds in four loaded runs. Both were the suite's own assertions reading the shared `kubectl.log` by position while the heartbeat patched the lock. The fix was 78a12fbe, test-only, and five loaded runs passed at b467bd82.
- Review round 10 found that no case reached the post-SIGKILL bound (thread PRRT_kwDORFy7ds6yntgX). `FAKE_RUNNER_EXEC_FAIL_AT_KILL` reached it (f02759fd).
  - It was red with the old `killed=1` shape: "that wait ended 0 s after its SIGKILL".
  - It was red with the branch removed: "that wait took 87 s after its SIGKILL to end".
