---
title: "A pod browser harness moves TMPDIR too, and the failure mode is a silent kill with no error naming it"
category: testing
tags:
  - issue-pod
  - playwright
  - postgres
  - tmpdir
  - ephemeral-storage
  - harness
  - eviction
date: 2026-10-20
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-44"
  - "INFRA-69"
  - "sjawhar/legion#2083"
  - "sjawhar/legion#2088"
---

# A pod browser harness moves TMPDIR too, and the failure mode is a silent kill with no error naming it

Extends docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md.

That document says to build the harness on `/legion` and to move `GOCACHE`, `GOMODCACHE` and
`PLAYWRIGHT_BROWSERS_PATH` with it. The deltas are the mechanism that makes it mandatory, the set
of paths that mechanism charges, and a fourth variable — all learned by paying for them.

**The mechanism, once.** Each container of an issue pod is held to its own limit of local ephemeral
storage. When one container passes *its own* limit the kubelet evicts the **whole pod** — every
other role's turn and session with it. That is why the death carries no disk message: no write
inside the container fails and no quota is reported; the process is killed and the container
filesystem comes back reset. Everything below follows from that one sentence.

**Every figure in this note is this pod's.** This pod was created before both of the evening's
ephemeral-storage changes, so it carries 1 GiB for every role, which is the state
dispatch://INFRA-51/comment/bf8035e7-9c34-4dcd-b3d7-4b787f815a1a records. That deployment has
changed its ephemeral-storage limits at least twice on 2026-10-20: the implementer's request and
limit went to 4Gi at 22:30Z (that comment, which also records that a raise applies only to pods
created after the apply), and every role's were rebalanced at 22:50Z
(dispatch://INFRA-51/comment/a9f52454-8972-41ef-aa39-a161af45f020). A pod keeps the values it was
created with, so a note's figure ages the moment an operator applies. Your own container's limits
are not readable from inside it: the only counter a role container can read is the sandbox's,
`/sys/fs/cgroup/memory/memory.limit_in_bytes` (why it is the sandbox's and not a limit is below).
They are in the pod spec, so the answer comes from whoever can read the cluster, or from the
deployment's own record — the two comments above.

## What the limit charges

Charged — one container filesystem, and all of these share a device:

- `/tmp`, which is not a mount in a role container at all (below), only a directory on that
  filesystem;
- the home directory `/home/legion` and everything under it that is not a mount;
- `/home/legion/.cache`;
- the Go module and build caches a build fills, which default under that home directory —
  `go env GOMODCACHE GOCACHE` prints `/home/legion/go/pkg/mod` and `/home/legion/.cache/go-build`;
- Oh My Pi's state home `/home/legion/.local/state/<role>`, with its Chromium profiles and logs.

Not charged:

- the issue volume `/legion`, and the read-only feed `/legion/feed` on it;
- the sessions directory `/home/legion/.omp/profiles/legion/agent/sessions`;
- the in-memory `emptyDir`s `/home/legion/.config`, `/var/run/legion/private` and
  `/var/run/legion/state`, which mount as `tmpfs rw`;
- `/var/run/legion/launcher` and `/var/run/legion/gh`, which are not in-memory directories but
  read-only credential mounts — Secret volumes, `9p ro`, which a reader cannot write to at all.

Free of the storage budget is not free: the three in-memory `emptyDir`s spend memory instead. A
`dd bs=1M count=100` into `/var/run/legion/private` moves `free -k`'s `shared` column from 132 to
102,532 KiB — exactly the 100 MiB written — and releases it when the file is removed. Both figures
were read in two role containers of this pod, this issue's implementer and its tester, and are
recorded on `sjawhar/legion#2088`. So a write into one of those three directories is resident
memory, and a reader who moves a cache onto
`/home/legion/.config`, `/var/run/legion/private` or `/var/run/legion/state` trades the eviction
for an OOM kill — the same silent death this note exists to prevent.

What this pod cannot show is whose budget that memory spends. The sandbox is cgroup v1, and
`ls /sys/fs/cgroup` is how a reader sees which layout their own container has: here it lists `cpu`,
`cpuacct`, `cpuset`, `devices`, `job`, `memory` and `pids`, and the counter is
`/sys/fs/cgroup/memory/memory.usage_in_bytes`. That counter is the sandbox's, not the container's:
`memory.limit_in_bytes` reads 20,401,094,656 bytes, equal to `free -k`'s total of 19,922,944 KiB,
and matches no role's own limit. Each role container has a memory limit of its own and they
differ. Legion's repository defaults are request equal to limit, 1Gi for the architect, the planner
and the merger, 4Gi for the reviewer, 6Gi for the implementer and the tester (`defaultResources`,
`packages/daemon/internal/config/kubernetes.go`, and the recorded pod shape
`packages/daemon/internal/runtime/sandbox/testdata/golden/root.json`) — defaults, not a promise
about a reader's container: an operator's `runtime.kubernetes.resources` sets what a pod actually
carries. On this deployment they are also what a pod carries, because its stack sets no memory at
all; a live issue pod created after the evening's applies was read with exactly those six values
(dispatch://INFRA-51/comment/12ce8e6b-0e14-4ff4-9cb2-b48306b15906). Either way the limits are the
pod spec's, not something the container can read.
The rule is Kubernetes' own, not verified from inside this sandbox, which can see no
per-container counter: for a memory-backed `emptyDir`,
"files you write count against the memory limit of the container that wrote them"
(https://kubernetes.io/docs/concepts/storage/volumes/#emptydir). So treat the write as spending
the role's own memory budget. The smallest memory limit recorded for a role is 1Gi — in Legion's
defaults, in the recorded pod shape, and in the live post-apply pod read earlier in this
paragraph — and a cache moved onto a tmpfs can exceed it.

**`/tmp` is not the exception a reader expects.** A role container mounts no `/tmp`: the
memory-backed `tmp` volume belongs to the `workspace-fetch` init container alone, as that
container's own `TMPDIR` (`packages/daemon/internal/runtime/sandbox/podkind.go`,
`packages/daemon/internal/runtime/sandbox/names.go`, and the golden pod JSON
`packages/daemon/internal/runtime/sandbox/testdata/golden/root.json`, in which the architect,
planner, implementer, tester, reviewer and merger containers mount the issue volume, the sessions
directory, `/home/legion/.config` and four `/var/run/legion/*` directories, and no `/tmp`).
`docs/kubernetes.md` states it in prose. A habit of "put the big download in /tmp" gets the pod
evicted here; nothing about `/tmp` in a role container is free.

**The admitted limit is not the number the configuration shows.** Every role's default reservation
asks for a much larger ephemeral-storage limit over a 1 GiB request — 20 GiB for the implementer
and tester, 10 GiB for every other role (`defaultResources`,
`packages/daemon/internal/config/kubernetes.go`), and an operator's
`runtime.kubernetes.resources` may raise them further. This GKE Autopilot cluster admits a
container's limit equal to its request and discards the configured one:
`gkeEphemeralStorageClamped` in
`packages/daemon/internal/runtime/sandbox/live_lifecycle_test.go`, whose doc comment records it
confirmed live on 2026-10-17 against every role container of a real production issue pod. On a
cluster that admits the configured limit the number differs; the mechanism does not.

- **Read the failure mode first, because the pod will not tell you.** Putting Postgres, the browser
  download or scratch on a charged path does not fail with a disk message, a quota error or anything
  naming storage. The process simply dies mid-work and the agent's turn ends; the next turn finds a
  container whose filesystem has been reset, and the obvious reading — a flaky command, a slow
  network, a timeout — is wrong every time. The kill is the eviction, and the eviction is recorded
  as a Kubernetes event on the pod, which nothing inside the container can read. Two downloads were
  killed this way before everything moved onto the issue volume. If a long command in a worker pod
  dies with no output and no status, suspect the container filesystem before anything else.
- **`TMPDIR` is a fourth variable, not covered by the other three.** INFRA-44 paid for this one:
  `apt-get download`, `dpkg-deb -x` and the Playwright browser install staged through `TMPDIR`
  there (`sjawhar/legion#2083`). No role container sets `TMPDIR`, so they default to `/tmp` —
  charged, out of the same budget as `$HOME` — and a harness whose caches are on the volume still
  writes a gigabyte against the limit unless `TMPDIR` is exported onto the issue volume with
  `GOCACHE`, `GOMODCACHE` and `PLAYWRIGHT_BROWSERS_PATH`. Export it in the same sourced env file as
  the rest.
  **Note 2026-10-25 (INFRA-221):** the premise "No role container sets `TMPDIR`" was true when
  this learning merged (sjawhar/legion#2088, 2026-10-21 03:32Z) and expired three hours later:
  since sjawhar/legion#2085 (06:29Z) the runtime sets `TMPDIR` to `/legion/scratch/tmp`, on the
  issue's volume, in every role container, with `GOCACHE` and `GOMODCACHE` beside it
  (`roleEnvironment`, `packages/daemon/internal/runtime/sandbox/manifest.go`), so no export is
  needed and only an explicit `/tmp` path still lands on the container's filesystem. A harness
  root staged through that `TMPDIR` and reached only by the harness's own commands meets both
  conditions of `skills/legion-worker/references/tools.md`, "The test: where it lands, and what
  it leaves resolving".

The same applies to any long-lived cache a role sets up for itself, not only this harness: a Go
build's caches land under `/home/legion` by default, which the limit charges exactly as it charges
`/tmp`. This change had to point `GOMODCACHE` and `GOCACHE` at the volume before a cold `go build`
of the Dispatch server could finish at all, because each eviction reset the container filesystem,
discarded the partial module download, and left the next attempt starting over.

## Evidence

INFRA-44's tester stood the whole stack up inside its own worker pod with everything on
`/legion` — PostgreSQL 17 with pgvector 0.8.0, `bun run build:web`,
`packages/dispatch/e2e/run-server.sh`, real Chromium — and got two full rounds green: round 1,
159 passed across seven specs; round 2, **299 passed, 21 skipped, 0 failed in 16.1 minutes**
across thirteen specs in chromium and iphone, plus seventeen throwaway fault-injection probes.
That is the measurement that made the implementer's `E2E (implementer)` line's premise false, and
it is why the rules above are worth their space: the recipe is cheap once the failure mode is not
mistaken for flakiness.

**The cluster read, dispatch://INFRA-51.** The eleven surviving eviction events of 2026-10-20 all
name the `implementer` container and the 1 GiB limit, and the live implementer held about 1.5 GiB of
Go module cache and 2.9 GiB of Go build cache under its home directory — about 4.4 GiB against
1 GiB. That read also said a role container's `/tmp` sits in memory and is therefore free. It is
neither, and the measurement below is the correction.

**The first in-pod reading** is
dispatch://INFRA-51/comment/0ce1448c-ccbc-4b5f-aaaf-60dce4c5eb05, taken at 22:20Z in this issue's
planner container and posted back on the cluster read: `/proc/mounts` had no `/tmp` line at all, and
`stat -c '%n dev=%d'` put `/tmp`, `/home/legion` and `~/.cache` together on device 83, the
container's own filesystem, with `/home/legion/.config` on 56 and `/var/run/legion/state` on 97
(both in memory), `/legion` on 87, the issue's volume, and `du -sx /` at 42 MB. That reading is what
corrected the cluster read's `/tmp` sentence.

**The figures below** were measured in this issue's implementer container on 2026-10-20 and are
recorded in this change's pull request `sjawhar/legion#2088` and its handoffs; they have been
reproduced since in the tester's and the reviewer's containers of the same pod. Each container has
its own overlay, so the device numbers differ from one container to another while the split they
show does not.

- `/proc/mounts` carries no `/tmp` line.
- `stat -c '%n dev=%d'` gives device 114 for `/tmp`, `/home/legion`, `/home/legion/.cache` and
  `/home/legion/.local/state` alike; 118 for `/legion`; 56 for `/home/legion/.config`; 128 for
  `/var/run/legion/state`; 130 for `/var/run/legion/private`; 124 for `/var/run/legion/gh`.
- `df -k --output=used /` read 14,392 KiB at the moment of that measurement. A 200 MiB write under
  `/tmp` took it to 219,192 KiB, and so did the same write under `/home/legion/.cache`; removing
  the file returned it to 14,392 KiB. The same 200 MiB written under `/legion`, or under
  `/home/legion/.config`, left the container filesystem where it was and grew that volume instead.
  The baseline is whatever the container happens to hold, so it is the delta that reproduces, not
  the figure: a later run in this same container read 38,280 → 243,080 → 38,280 KiB for the same
  probe, exactly the 204,800 KiB written each time.
- **`du` sees these files; `du -x` does not.** Pointed at a path, `du` reports a written file
  normally: with a 200 MiB file under `/tmp`, `stat -c 'blocks=%b' /tmp/probe` prints
  `blocks=409600` — 200 MiB in 512-byte blocks, not 0 — `du -sk /tmp/probe` prints `204800`, and
  `du -sk /tmp` prints that plus whatever else the directory holds (`217364` at that run).
  What is blind is the one-filesystem walk, `du -x`: on this gVisor
  overlay a written file reports a different device from its parent directory —
  `stat -c '%n dev=%d'` gives `/tmp` dev=114 but `/tmp/probe` dev=112, and `/home/legion/.cache`
  dev=114 but a file written in it dev=112 — so `du -x` treats every written file as living on
  another filesystem and skips it: `du -sx -k /` stays `42348` across the write,
  `du -x -a -d1 -k /` prints `0` for `/tmp`, and `du -sx -k /home` stays `13516` with 200 MiB under
  `~/.cache`. So use `du -sh ~/.cache`, or `du -sh` on any directory, without `-x`, to find what a
  directory holds — it reported `212M` with that probe in place — and `df -k --output=used /` to
  measure the whole container filesystem against the limit; `du -x` is the one to avoid here. The
  `df` caveat is a different one: `df` answers for the filesystem that holds the path it is given,
  so a path under a volume measures that volume and says nothing about the 1 GiB
  container-filesystem budget — `df -k --output=target,used /legion` names `/legion`, the issue
  volume — while a path the container filesystem itself holds names `/`, the budgeted one:
  `df -k --output=target,used /home/legion/.cache` and `df -k --output=used /` report the same
  filesystem and the same figure. Verified twice, in two different role containers of this pod.

The limit of all of it: a role container carries no ServiceAccount token and reaches no API server.
The pod runs under the **operator's** ServiceAccount with `automountServiceAccountToken: false`, so
no token is mounted: `packages/daemon/internal/runtime/sandbox/live_operator_test.go` asserts
both and execs the pod to confirm the token file is absent. `/var/run/secrets/kubernetes.io` does
not exist, and `kubectl get --raw /api` answers `The connection to the server localhost:8080 was
refused`. So neither the kubelet's per-container accounting nor the eviction event itself can be
read from inside a role container: measure from inside with `df` and `du`, and read the event from
the cluster.
