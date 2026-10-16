---
title: "A live stall drive freezes whole process groups (or makes the work outlast it) and allows for the rig's one PID namespace; a shared pod log is parsed from the writer's own line start and never decides the outcome"
category: testing
tags:
  - process-group
  - sigstop
  - sighup
  - gvisor
  - pid-namespace
  - pod-log
  - untrusted-input
  - log-parsing
  - stage-4b
  - codegraph
date: 2026-10-16
status: active
module: scripts/e2e/stage4b-sandbox-tree.sh, packages/daemon/internal/workspace/codegraph.go
applies_when:
  - A proof must hold one process of a `setsid` group still (SIGSTOP) while another member keeps running, in a worker pod (gVisor)
  - A rig runs in one PID namespace what production runs in several (the six role containers of an issue pod)
  - A live proof reads a line one process logs from a stream other writers share (a pod's container log carries the shim's lines and the agent's frames)
  - A once-per-event log line is proposed as the signal a checkpoint waits on
related_issues:
  - "LEGION-638"
  - "sjawhar/legion#1981"
---

# A live stall drive freezes whole groups or makes the work outlast it; a shared pod log is parsed from the writer's own line start

Extends `docs/solutions/testing/prove-a-cross-process-filesystem-property-on-the-filesystem-it-runs-on-and-through-the-real-stop-signal-LEGION-629.md`.
That note's process-group rule covers the real stop (`kill -TERM -- -<pgid>`); this is the
STOP/CONT side of the same rule, and the parsing side of a proof that reads a pod's log.

- **SIGSTOP a whole group, never one member of a `setsid` group whose leader runs on.** Under
  gVisor, stopping only the `codegraph` children of a shim started with `setsid` ended the shim
  with SIGHUP: a stopped member in an orphaned process group (no parent in the session) triggers
  the POSIX SIGHUP+SIGCONT rule. `kill -STOP -- -<pgid>` of the whole group, as the takeover drive
  does, is safe; when the holder must keep ticking while its work stands still, do not freeze at
  all — **make the work outlast the drive** instead (four copies of the repository took the real
  `codegraph init` from 29 s to about two minutes, long enough for two 10 s ticks plus margin).
- **Allow for the rig's one PID namespace.** In a pod the six role containers cannot see each
  other's PIDs, so a successor reads a frozen holder's `codegraph.lock` as dead and builds; in a
  rig they share one namespace, the successor finds that PID alive and skips, releasing its lease at
  once. A stalled-holder drive therefore freezes the successor the instant its token is in the lease
  (poll the file's content every few milliseconds), so the successor holds its lease for the
  observation; say in the proof that this stands in for the namespace boundary.
- **Parse a shared log from the writer's own line start.** `kubectl logs --all-containers
  --timestamps --prefix` gives `[pod/<pod>/<container>] <RFC 3339 ns> <line>`, and the same stream
  carries the agent's frames, whose arguments the agent chooses (`tool_execution_start bash
  {"command":"echo [legion] codegraph warm-up for …"}`). Anchor the filter and the parser on the
  prefix, the timestamp and the writer's own first words (`[legion] codegraph warm-up for `), in the
  `grep -E` and in the shell `case` (a `case` pattern matches the whole word, so its leading literal
  anchors it), and put a forged line from another container **before** the real one in the test
  fixture. The anchor is hardening, not a boundary: the writer's own `log.Print` of decoded strings
  can still be split by a newline in them, so the line decides attribution and timing only.
- **Never let the parsed line decide the outcome.** The checkpoint's pass is `codegraph status
  --json` reporting `initialized: true` and `index.state: "complete"`; the success line names the
  container that built and the seconds from the pod's creation. Its absence is normal in cases the
  checkpoint cannot tell apart — a relaunch onto a complete index logs nothing, a container
  restarted before the checkpoint never logs it again, a build ending with its context logs `ended
  with its process` instead — so the wait that times out names the evidence file and the status
  read, not the line.

## Evidence

- LEGION-638 round 1 (sjawhar/legion#1981): the first unreadable-lease drive stopped the two
  `codegraph` processes alone; bash reported the shim `Hangup` within a second, both runs read "the
  heartbeat is dead", and the lease was left behind — a drive that proved nothing. The four-copy
  workspace (17 314 files) kept the real build running for the 10 s control tick, the 25 s the lease
  was away and the tick that refreshed it (`03:41:19` → away → back 30 s old → `03:41:49`).
- The takeover drive froze the holder's whole group 2 s into its build, and the successor's whole
  group the instant its token landed; the successor's lease then read the same token, mtime and
  inode (83657, reused on 9p) over two of the resumed holder's ticks.
- The stage 4b parser at `scripts/e2e/stage4b-sandbox-tree.sh` (`codegraph_warm_up_lines`,
  `codegraph_built`, `warm_up_built_elapsed`) and `scripts/e2e/lib/stage4b-codegraph-warm-up.test.ts`:
  the pre-anchor parser answered the forged implementer line; the anchored one answers the reviewer's
  real line placed after it, and exits 1 with nothing printed when only the forged line is present.
