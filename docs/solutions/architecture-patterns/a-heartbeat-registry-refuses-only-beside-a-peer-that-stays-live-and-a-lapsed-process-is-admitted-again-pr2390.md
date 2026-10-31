---
title: "A heartbeat registry refuses only beside a peer that stays live, and a lapsed process is admitted again before it counts as live"
category: architecture-patterns
tags:
  - registry
  - heartbeat
  - admission
  - postgres
  - advisory-lock
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/processes
---

# A heartbeat registry refuses only beside a peer that stays live, and a lapsed process is admitted again before it counts as live

When processes admit each other through rows that a heartbeat keeps live (a boot check that one
process may not serve beside another), the boot check alone is not the contract. Apply all of:

- **Refuse only beside a peer that stays live.** A process killed without deregistering (SIGKILL,
  out of memory, a lost node) leaves a row that reads live until its window passes. On a refusal,
  retry every heartbeat for one live window plus one heartbeat, and refuse only if the refusing
  rows are still live then. Otherwise a restart after a kill is refused by a process that no
  longer runs.
- **A process whose own row lapsed runs the admission check again before it stamps itself
  live.** While its heartbeat had lapsed (frozen, cut from the database, its row pruned), every
  process that registered judged it gone and checked nothing against it. The heartbeat that finds
  its row stale or missing takes the same lock as registration, checks the live peers, and either
  rejoins or stops serving (drain, exit 1). Only a heartbeat that finds its row still live may
  just stamp it.
- **Measure "lapsed" after taking the lock, with `clock_timestamp()`, not `now()`.** `now()` is
  the transaction's start, earlier than the lock; a registrant that judged the row stale by its
  own `now()` must never be followed by a renewer that judges the same row live.
- **Use one admission function for both paths** (registration and the lapsed heartbeat), so a new
  column or refusal text cannot land in one and not the other.
- **A stop during the admission wait is a clean stop** (exit 0): the process served nothing.
- **Every test or harness that starts a second process on the same database is now subject to
  the check.** Give such processes the shared credential, or rely on the wait for a restart after
  a kill; check the memory, restart and multi-process harnesses when you add the rule.

## Evidence

- `packages/envoy/internal/dispatch/processes/processes.go`: `waitForAdmission` (line 211) is the
  retry; `Registration.renew` (line 375) takes `pg_advisory_xact_lock` and then reads
  `heartbeat_at > clock_timestamp() - …`; `admit` is shared by `Register` and `renew`.
- The tester's red test `TestAProcessThatResumesBesideAPeerWhoseKeySetItDoesNotShareDoesNotServeBesideIt`
  (`packages/envoy/cmd/dispatch/multiprocess_test.go`) froze a process with SIGSTOP past the
  15-second window, started a peer with another signing key, and resumed the first. At 2f16634c
  both kept serving and refused each other's cookies; the fix is 966b5b60.
- Review found the second gap: a dev sign-in server (its key minted per start) restarted right
  after a SIGKILL was refused beside its own dead row (exit 1 at e1754f3c). The wait (5e17af00)
  starts it about 15 seconds later; `TestADevSignInServerRestartedAfterAKillStarts` guards it.
- The census for the same rule found `cmd/dispatch/memory_harness_test.go` starting two servers
  that each minted a key; without one shared `DISPATCH_SIGNING_KEY` (718a8fa4) the memory job
  would have failed. `edit_notice_restart_test.go` needed the same.
- The stop during the wait answered exit 1 until 62a8f2d1 (`TestAProcessStoppedWhileWaitingForAdmissionExitsCleanly`).
