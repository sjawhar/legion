---
title: "A role's scratch on the container filesystem is what an issue pod is replaced over: measure it, then move it to the issue volume"
category: legion
tags:
  - ephemeral-storage
  - issue-pod
  - eviction
  - agent-sandbox
  - harness
date: 2026-10-20
status: active
module: packages/daemon/internal/runtime/sandbox
related_issues:
  - "INFRA-43"
  - "sjawhar/legion#2084"
---

# A role's scratch on the container filesystem is what an issue pod is replaced over: measure it, then move it to the issue volume

Extends docs/solutions/legion/a-gvisor-pods-reservation-is-the-pods-sum-qos-reads-cpu-and-memory-alone-and-ephemeral-storage-is-an-eviction-fence-over-a-small-request-LEGION-632.md.

That document states the mechanism: ephemeral storage is the node's disk a container's root
filesystem writes to, the limit is an eviction fence, and a container past it has its **whole pod**
evicted — every role of the issue with it. This is what that costs a working role, and what to do
about it.

- **A role's scratch is bigger than it looks, and it is all on that filesystem by default.**
  `$HOME/.cache`, `/tmp`, the Go build and module caches, a browser download, a database's data
  directory. One browser-and-database harness measured 297M `GOCACHE`, 192M `GOMODCACHE`, about
  400M of Chromium and about 300M of Postgres packages and `PGDATA` — roughly 1.2G against a cap
  of about a gigabyte.
- **Move it to the issue volume (`/legion`) before you build it, not after the first loss.** The
  volume survives a pod replacement; the container filesystem does not, so a replacement costs the
  rebuild as well as the eviction. Each tool takes one variable: `GOCACHE`, `GOMODCACHE`,
  `PLAYWRIGHT_BROWSERS_PATH`, a `PGDATA` path, an `apt-get` state and cache directory.
- **Read a pod replacement as a measurement, not an accident.** A turn that comes back with
  `/tmp` and `~/.cache` empty while the jj workspace is intact is a replaced pod: say so, with
  what your role had written there, rather than rebuilding quietly.
- **Nothing here reads a cluster eviction event.** A role cannot; treat the correlation as
  evidence and name its limits where you report it.

## Evidence

One issue pod on 2026-10-20, running a Playwright-and-Postgres harness for INFRA-43: replaced
seven times in six hours while that harness lived under `/tmp` and `~/.cache` (twice mid-turn,
each time with the workspace intact and the scratch gone), and not once in the seven hours after
it moved to `/legion/harness`, through the heaviest part of the issue — two full browser suites,
several SPA builds, a Go build and the tester's own runs.

What that is not: no one inside Legion read a Kubernetes eviction event for any of those
replacements, the window is one pod on one day, and no other cause was ruled out. The fix and the
decision for the fleet are dispatch://INFRA-31, which is where the cap and the per-pod accounting
belong; this file records only what a role can measure from inside its own pod, and the move that
is cheap enough to make before the evidence is complete.
