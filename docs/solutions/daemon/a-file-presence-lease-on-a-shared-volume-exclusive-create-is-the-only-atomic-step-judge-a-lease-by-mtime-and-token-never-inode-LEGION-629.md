---
title: "A file-presence lease on a shared volume: the exclusive create is the only atomic step, so a stale takeover runs under its own marker, and a lease is judged by its mtime and known by its token, never by its inode"
category: daemon
tags:
  - lease
  - cross-process
  - gvisor
  - v9fs
  - inode-reuse
  - o-excl
  - stale-takeover
  - codegraph
  - worker-shim
date: 2026-10-08
status: active
module: packages/daemon/internal/workspace/codegraph.go
applies_when:
  - Processes in different pods (or containers with their own PID namespaces) must not both act on one directory of a shared volume
  - flock cannot be the guard, because the contenders are not in one kernel's view of the file (under gVisor a pod's flock never reaches another pod)
  - A pid-file or lease protocol needs a stale-holder takeover and you are reaching for rename, link or os.SameFile
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
---

# A file-presence lease on a shared volume: exclusive create, a takeover marker, mtime and a token — never an inode

Extends `docs/solutions/daemon/instance-lock-is-a-kernel-flock-not-a-pid-file.md`: that document
is right that a pid-file protocol with a rename-aside takeover lets two contenders hold the lock,
and right to hand the lock to the kernel with `flock(2)` — **where every contender is in one
kernel's view of the file**. The CodeGraph warm-up's contenders are not: six role containers of
one issue pod, and a draining pod beside its replacement, each warm one workspace on the tree
volume, and under gVisor a pod's `flock` never reaches another pod
(`packages/daemon/internal/runtime/sandbox/manifest.go`, the `initWaitSeconds` comment). A file's
existence and mtime on the shared volume do reach every pod. When that is the situation, the
protocol is:

- **The only atomic step is the exclusive create** (`O_CREAT|O_EXCL`). A rename is not one: it
  moves whatever is at the path at that instant, a winner's fresh lease included. Nothing in the
  protocol may decide a winner by a rename, a link, or a read-then-write.
- **A stale takeover runs under a second exclusive create**, a marker beside the lease. Of two
  contenders that both judged the same lease stale, exactly one creates the marker; the other
  finds it and leaves the takeover to the holder. Under the marker the lease is judged again
  before it is removed, since the marker's winner may already have replaced it. A marker older
  than the staleness window is a contender killed inside those few syscalls; remove it and retry
  once.
- **Judge a lease by its mtime, never by its identity.** `os.SameFile` compares device and inode,
  and the tree volume (v9fs over the issue's volume) hands a file created at a path the inode the
  removed one had — so the stale file and the winner's fresh lease at the same path *are the same
  file* to `os.SameFile`, and the loser removes the live lease from under the winner's build.
  Under the marker: older than the window → the dead holder's, remove; younger → a new holder's,
  leave; gone → released. The caller's create decides afterwards.
- **A holder knows its own lease by a token it wrote into it** at the create, and releases only
  while the file still holds that token: a holder stalled past the window and taken over must not
  remove its successor's lease, and an inode comparison there has the same reuse hazard.
- **Keep the heartbeat and the window honest.** The holder refreshes the lease's mtime every
  `warmLeaseHeartbeat` (10 s); `warmLeaseStale` (60 s, six missed beats) is what a holder killed
  outright costs the next contender. Everything else — in-process de-duplication, a PID check of
  some other tool's lock — is process-local and decides nothing across pods.
- **When the same hazard is found at one site, check every site in the family.** The inode
  comparison was found by the tester in the takeover and fixed; the release guard three functions
  down used `os.SameFile` too, and the tester's next round found that one.

## Evidence

- LEGION-629 built the lease three times. Round 2 took a stale lease over by rename; the reviewer
  showed two contenders that both judged one lease stale could both hold it (B's rename moves A's
  fresh lease aside, since rename fails only when the path is absent). Round 3 added the takeover
  marker and re-judged under it with `os.SameFile`; the tester showed the same two-contender
  interleaving still double-built on the tree volume, measured: `touch; rm; touch` on
  `/legion/workspaces` (v9fs) gives inode `95 → 95`, on `/tmp` (overlayfs) `50962 → 50963`. Round 4
  judges by mtime and identifies by token; the same test, with its TempDir on the tree volume,
  passes, and two real shims started together over a 5-minute-old lease on a v9fs clone produced
  one builder in six of six runs, the fresh lease reusing the stale one's inode every time.
- The release guard's `os.SameFile` was the same finding's second site, caught a round later
  (tester's `TestAStalledHoldersReleaseLeavesItsSuccessorsLeaseAtTheSameInode`).
- The one case the token does not cover is a mixed-version rollout: a draining pod on a pre-fix
  `legion` releasing by `os.SameFile` beside a replacement on this one. One rollout only.

## Related

- `docs/solutions/daemon/instance-lock-is-a-kernel-flock-not-a-pid-file.md` — the flock rule,
  where flock applies, and the pid-file rename hazard this protocol also had to avoid.
- `docs/solutions/testing/prove-a-cross-process-filesystem-property-on-the-filesystem-it-runs-on-and-through-the-real-stop-signal-LEGION-629.md`
  — how the two findings above were proven and would have been caught earlier.
