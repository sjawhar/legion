---
title: "A watchdog under errexit guards every kill and ends the run through one function that records why"
category: testing
tags:
  - bash
  - errexit
  - watchdog
  - live-proof
date: 2026-10-29
status: active
module: scripts/e2e
related_issues:
  - "sjawhar/legion#2186"
---

# A watchdog under errexit guards every kill and ends the run through one function that records why

- A background watchdog subshell inherits the caller's `set -e`. A `kill` of a process that is
  already gone exits non-zero and ends the watchdog before it signals anyone, so the run goes on
  and fails later at an unrelated check. Put `|| true` on every command on its fatal path, and
  in an `EXIT` trap, where a failing `kill` changes the exit status too.
- Route every fatal path through one function. It returns at once once teardown has begun (a
  stopping flag the normal stop writes), writes the reason and the evidence of what removed the
  resource (the pod's last events, or its node's) to a file the caller's `FAIL` line reads, stops
  what the run drives, and signals the run's own shell so its `EXIT` trap tears everything down.

## Evidence

`scripts/e2e/lib/bridge-tunnel.sh` (`bridge_down`, the watchdog). A sandbox proof's bridge pod was
preempted by a system pod on a full node; the watchdog died at `kill "$pf_pid" ...` under errexit
and the run failed minutes later at an unrelated timeout. After the fix (commit 1d7540ed) the run
ends at once with `FAIL bridge: <reason>; the bridge pod <name>: <its events>`.
