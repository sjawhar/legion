# Legion's resource sampler: the series it serves

`legion sampler` reads what the kernel already counts and serves it as Prometheus series: in an issue pod, each role's CPU and memory split by the kind of work its processes do, each volume's used bytes and the pod's own totals;
on a grade node (`legion sampler --node`), the same split for each pod of the node.
This file is the contract the rest of dispatch://INFRA-1141 is built on: the scrape, the recording rules and the dashboards pin the names, the labels and the rules below.

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
`bun scripts/semantic-line-breaks.ts docs/sampler-metrics.md` checks the layout, and `--write` fixes it.

The sampler's code is `packages/daemon/internal/sampler`, and the series registry this file describes is its `series.go`.
A test reads the three tables below and compares them with the series the registry serves, in each mode, so this file and the code cannot drift apart.

## The port

The sampler binds `0.0.0.0:9464` and serves its series at `GET /metrics`.
The issue pod's `roles` container declares that port as `sampler`, numbered 9464.

The bind address is every address and not loopback because the collector runs outside the pod and reaches the port at the pod's own address.
A sampler bound to `127.0.0.1` would answer the pod's own processes and nobody else, and every check that reads the port from inside the pod would still pass, so the mistake would reach production unseen.

## The series an issue pod serves

Every series below carries `project` and `issue`.
The daemon reads them from the pod's `legion.dev/project` and `legion.dev/issue` label values and passes them to the sampler on the command line.
There is no `pod` label: Managed Prometheus adds one itself.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `legion_sampler_cpu_seconds_total` | counter | `project`, `issue`, `role`, `kind` | CPU seconds this role has spent on work of this kind |
| `legion_sampler_memory_anonymous_bytes` | gauge | `project`, `issue`, `role`, `kind` | anonymous memory this role's work of this kind holds now |
| `legion_sampler_memory_anonymous_peak_bytes` | gauge | `project`, `issue`, `role`, `kind` | the most anonymous memory it has held since the last scrape |
| `legion_sampler_memory_resident_bytes` | gauge | `project`, `issue`, `role`, `kind` | resident memory this role's work of this kind holds now |
| `legion_sampler_memory_resident_peak_bytes` | gauge | `project`, `issue`, `role`, `kind` | the most resident memory it has held since the last scrape |
| `legion_sampler_volume_used_bytes` | gauge | `project`, `issue`, `volume`, `medium`, `role` | bytes used in this volume, read once however many roles mount it |
| `legion_sampler_pod_memory_bytes` | gauge | `project`, `issue` | memory the pod's cgroup holds now |
| `legion_sampler_pod_memory_peak_bytes` | gauge | `project`, `issue` | the most the pod's cgroup has held since the last scrape |
| `legion_sampler_pod_cpu_seconds_total` | counter | `project`, `issue` | CPU seconds the pod's cgroup has spent |
| `legion_sampler_pod_process_memory_bytes` | gauge | `project`, `issue` | memory the pod's processes hold: `AnonPages`, plus `Shmem` on a kernel that keeps them apart |
| `legion_sampler_pod_process_memory_peak_bytes` | gauge | `project`, `issue` | the most the pod's processes have held since the last scrape |
| `legion_sampler_pod_shared_memory_bytes` | gauge | `project`, `issue` | shared memory the pod holds now, the `Shmem` of its `/proc/meminfo` |

`medium` is what the pod spec declares for the volume, `memory` or `disk`, and never what `/proc/mounts` shows:
a disk `emptyDir` shows there as `tmpfs`.
`role` on a volume is the role that owns it, or `shared` for a volume no single role owns.

A volume's bytes are one `statfs`, of the first of the volume's paths that can be read, and a `statfs` reports the filesystem that path is on rather than the directory's own tree.
In an issue pod under gVisor the two are the same thing: the kernel there gives each `emptyDir` a tmpfs of its own, so the filesystem is the volume.
On a kernel that backs a disk `emptyDir` with a directory of the node's own filesystem they are not, and the series would carry the node's whole used disk rather than the volume's bytes.

## The series node mode serves

`legion sampler --node` runs as a DaemonSet on a grade node and serves one set of these for each pod of that node.
`queue` is the pod's `kueue.x-k8s.io/queue-name` label and `owner` its `owner` label, each read from the pod itself or, where the pod carries neither, from its owning Job.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `legion_sampler_grade_cpu_seconds_total` | counter | `grade_pod`, `grade_namespace`, `kind`, `queue`, `owner` | CPU seconds this pod has spent on work of this kind |
| `legion_sampler_grade_memory_resident_bytes` | gauge | `grade_pod`, `grade_namespace`, `kind`, `queue`, `owner` | resident memory this pod's work of this kind holds now |
| `legion_sampler_grade_memory_resident_peak_bytes` | gauge | `grade_pod`, `grade_namespace`, `kind`, `queue`, `owner` | the most resident memory it has held since the last scrape |
| `legion_sampler_grade_pod_memory_bytes` | gauge | `grade_pod`, `grade_namespace`, `queue`, `owner` | memory this pod's cgroup holds now |
| `legion_sampler_grade_pod_memory_peak_bytes` | gauge | `grade_pod`, `grade_namespace`, `queue`, `owner` | the most this pod's cgroup has held since the last scrape |
| `legion_sampler_grade_pod_cpu_seconds_total` | counter | `grade_pod`, `grade_namespace`, `queue`, `owner` | CPU seconds this pod's cgroup has spent |

Node mode reads no `smaps`, so it serves no anonymous memory:
a node's sampler walks every pod on the node, and that read costs 2.45 ms a process.
It serves no volume series either, since nothing tells a node's sampler what another pod's mounts are.

## The series the sampler serves about itself

Both modes serve these, and they carry no `project` or `issue`.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `legion_sampler_self_cpu_seconds_total` | counter | — | CPU seconds the sampler itself has spent |
| `legion_sampler_self_memory_anonymous_bytes` | gauge | — | anonymous memory the sampler holds, the reading it exits on at 64 MiB |
| `legion_sampler_self_memory_resident_bytes` | gauge | — | resident memory the sampler holds, which under gVisor counts the mapped 102 MB `legion` binary |
| `legion_sampler_read_errors_total` | counter | `source` | reads of this source that failed: `proc`, `cgroup`, `volume` or `kubernetes` |
| `legion_sampler_restarts_total` | counter | — | times the launcher has started the sampler again, a count the launcher keeps and hands over |
| `legion_sampler_processes_seen_total` | counter | — | new processes the walk has read, counted at the `stat` read that first sees one |
| `legion_sampler_series_dropped` | gauge | — | series the 200-series cap removed at the last scrape |

All four sources of `legion_sampler_read_errors_total` are served from the first scrape, at zero, so a rate over one of them answers on a sampler that has never failed a read.

`legion_sampler_processes_seen_total` is the rate the cap below is held at: a rate over it is how many new processes a second the pod is starting, which is what the sampler's own cost grows with once its walks and its totals are at their floors.
Both modes serve it, since a node's sampler walks that node's whole process table and its reader needs the same number.

### The deferral series, which an issue pod's sampler alone serves

These three say what the cap on the sampler's own cost made it put off, and only an issue pod's sampler serves them:
the cap is over what the sampler costs the pod it reads, and a node's sampler reads no pod of its own and defers nothing, so a node that served them would serve three numbers that are always zero.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `legion_sampler_smaps_deferred` | gauge | — | processes the `smaps` budget carried into the next window: a level, how many are waiting now |
| `legion_sampler_totals_deferred` | counter | — | readings of the pod's memory totals the sampler's own cap has deferred, a count that only rises |
| `legion_sampler_walk_deferred` | counter | — | walk passes the sampler's own cap has deferred, a count that only rises |

The two counts carry no `_total` suffix, which the 0.0.4 text format does not require, because dispatch://INFRA-1208's spec gives these names and the dashboards of dispatch://INFRA-1141 pin them.

The cap on what the sampler costs the pod it reads is 3% of one CPU over any minute in which the pod starts at most 20 new processes a second, whether or not the pod's CPUs are saturated,
over the whole of its own work and not over one pass of it.
It is held from `/proc/self/stat`, as every other reading here is read from the kernel's own counts.
Each ten-second window has 250 ms of that allowance, five sixths of the cap, and what the window has spent already decides what the rest of it may read.
The window budgets against five sixths and not against the whole of it because the floors below force readings a spent allowance cannot pay for, and the sixth left over is what those cost.
Above that rate of new processes the cost grows with the rate: once the walks and the memory totals are at their floors nothing is left to give way, and each new process the walk meets still costs a `stat` on the pass that first sees it and a command line on the next.
`legion_sampler_processes_seen_total` is where a reader sees the rate a minute was held at.

The rates are split by what a reading costs rather than by what it is of:

- the pod's own CPU — the cgroup's `cpuacct.usage` under cgroup v1, its `cpu.stat` under v2 — is read every second, and the cap never slows it;

- the pod's memory totals — the cgroup's `memory.usage_in_bytes` (`memory.current` under v2) and `/proc/meminfo`, which feed `legion_sampler_pod_memory_bytes`, `legion_sampler_pod_process_memory_bytes` and `legion_sampler_pod_shared_memory_bytes` — are read every two seconds;

- every process's `stat` and every volume are read every two seconds;

- the `smaps` pass runs once every ten seconds.

The memory totals are the dear readings, and on a gVisor pod they are nearly all of what the sampler costs at all:
the kernel there accounts for the pod's memory on each read of it,
so the cgroup's `memory.usage_in_bytes` answers in 17 ms to 20 ms and `/proc/meminfo` in 12 ms to 20 ms,
against 0.50 ms for the cgroup's `cpuacct.usage`, 0.50 ms for the sampler's own `smaps` and 0.00 ms for its own `stat`.
That is why the two halves of the pod's totals are read apart:
the cheap half holds the second a dashboard reads a pod's CPU at, and the dear half is read at the two seconds the cap pays for.

What gives way where the allowance runs out is the reading rate and never the cap, in this order:

- the pod's memory totals fall from one reading every two seconds to one every four, and each reading that waits is counted as `legion_sampler_totals_deferred`;

- a walk pass, which reads every process's `stat` and every volume, waits for a later pass whenever the window has less left than the last walk cost, as often as the window needs, and each pass that waits is counted as `legion_sampler_walk_deferred`;

- the `smaps` pass spends what those two leave of the window's 250 ms and never more than 100 ms, and a window whose other work has already spent the allowance reads no `smaps` at all.

Under all three stands one floor: nothing waits longer than a window.
A window whose allowance is spent still runs one walk and one volume read before it ends, and the memory totals never wait longer than four seconds,
so every process's `stat` is read at least once each ten seconds and no series the sampler serves goes staler than that.
`legion_sampler_smaps_deferred` is how many processes a window carried into the next one, so a window that reached none of them says so there, and the next window takes the carried processes up first.

Each window ends on its own ten-second tick, whether or not the sampler read any `smaps` in it, and the next window's allowance begins there.
The allowance is the whole sampler's and not the `smaps` pass's, so the cgroup profile below, which reads no `smaps` at all, budgets its walk and its totals against a window that ends on the same tick.

The floor is a reading rate that never gives way, and one walk of a pod running hundreds of build processes can cost more than the 250 ms itself.
A window that goes over its allowance that way takes what it overspent out of the next window, up to one whole allowance,
since the cap is over a minute and not over one window of it:
the window that pays reads nothing it is not held to, and the sampler is back at its rate as soon as the debt is paid.

The walk is the reading a busy pod costs the sampler most:
a build makes thousands of short-lived processes, each one a `stat` to open, and under gVisor a syscall costs several times more while the pod's own work is saturating its CPUs.
So the two deferral counts are where an operator reads that a pod is busy enough to cost the sampler its cap,
and the answer to why that pod's readings arrived more slowly over the minutes the sampler was deferring.

The memory totals are what an idle pod costs the sampler most.
At the rates above, scraped every two seconds, this pod's sampler held 0.95% to 1.37% of one CPU over every rolling minute of three idle minutes and deferred nothing,
and 2.00% to 2.53% over every rolling minute of a `go build -a -o /dev/null ./...` that saturated both its CPUs for four minutes, where it deferred ten walk passes and one reading of the pod's memory.
Reading no totals at all it held 0.25%.
Scraped four times a second over an idle minute, `legion_sampler_pod_memory_bytes` moved 31 times, never more than 2.01 s apart.
A scrape of the port costs the sampler 1.3 ms, of which writing the exposition is 0.04 ms, so a collector reading every two seconds costs it 0.07% of one CPU.
Those are this pod's numbers, measured 2026-11-05 under dispatch://INFRA-1208.

The tester measured the bound itself on 2026-11-05, on this pod and on a `legion` built from `5fe2698c6aca`, scraped every two seconds:
a gVisor issue pod of two CPUs, which is the pod every number above was measured in too.
A `go build -a -o /dev/null ./...` there started about 11 new processes a second, and the highest of its 102 rolling minutes was 2.558%, none of them over 3%.
Four shell loops forking `date` started about 210 a second, and over two runs the highest rolling minute was 3.368% and 3.376% read from the series and 3.368% and 3.532% read from `/proc`, with the walk deferred on 42 of some 55 walk ticks and the memory totals on 26 of some 55.
That is the cost growing with the rate of new processes once the walk and the totals are at their floors, which is what the bound on the cap says it does.
Four CPU loops that fork nothing held the pod at 1.49 of its 2 CPUs and left the sampler at 0.910% with nothing deferred, so the term is the rate of new processes and not the pod's CPU load.

## The two profiles

The sampler reads two kinds of pod.
The metric names and the label names are the same in both, and only the source of each number differs, so one dashboard reads both.

### The `/proc` profile

This is a gVisor pod, which is what Legion's issue pods run today.
The pod's user cannot make a cgroup there, so the split comes from `/proc`:
each process's `stat` gives its CPU and its resident pages, its `smaps` gives its anonymous memory, and a process belongs to the role whose `role-init` process is itself or one of its ancestors.
A process in no role's subtree is role `shared`.

The pod's totals come from the cgroup v1 files at the mount root, `memory/memory.usage_in_bytes` and `cpuacct/cpuacct.usage`, and its process memory from `/proc/meminfo`:
`AnonPages`, which under gVisor already holds the tmpfs pages, plus `Shmem` on a kernel that keeps the two apart.
The sampler never reads `/proc/self/cgroup` to find the layout:
in this pod that file names a directory that exists nowhere on the filesystem.

### The cgroup profile

This is a pod that runs each command in a cgroup v2 cgroup of its own, named `<role>-<id>` under the directory `--cgroup-root` names.
Each command's cgroup gives that command's own numbers:
`cpu.stat`'s `usage_usec` is its CPU, `memory.current` its resident memory, `memory.stat`'s `anon` its anonymous memory and `memory.peak` its peak, and the pod cgroup's `memory.current` and `cpu.stat` are the pod's totals.
The role is the first part of the cgroup's name and the kind is the first `build` or `test` process found in it, else `tool`.

A pod of this profile runs on a kernel of its own, and that is the kernel a volume's `statfs` answers for:
where a disk `emptyDir` there is a directory of the node's filesystem rather than a tmpfs of its own, `legion_sampler_volume_used_bytes` carries the node's whole used disk and whoever deploys that pod must read it as such.

Nobody writes those cgroups today:
dispatch://INFRA-1115 leaves INFRA's pods on gVisor, so the sampler has the reading side and no pod yet has the writing side.
The section below is the contract whoever writes it must meet.

## The kinds

`kind` is what a process's CPU and memory are counted as, and the two modes use two vocabularies.

An issue pod:

- `agent` — a role's own machinery: its Oh My Pi process, the program the role runs and Legion's own processes for that role.

- `build` — a compiler, a linker or a build driver.

- `test` — a test runner, and the builds it does on the way.

- `tool` — everything else an agent runs: a language server, a formatter, a shell.

- `shared` — the launcher, the sampler itself and the test Postgres, which sit in no role's subtree and are served under role `shared` as well.

Node mode: `build`, `test` and `other`.
A node's sampler knows nothing of roles, so everything that is neither a build nor a test is `other`.

A rule list decides `build` and `test` from a process's program and its arguments, and a process under a `build` or a `test` process takes that kind.
A rule answers one of `build`, `test` and `tool` and no other word: `agent` and `shared` are decided by where a process sits in the process tree, and `other` is node mode's answer for the work no rule named, which an issue pod has no word for.
The rules are in `packages/daemon/internal/samplerwire/rules.go`, and a deployment adds its own with the daemon configuration key `runtime.kubernetes.sampler_rules`.

An issue pod's sampler reads a process's command line on the second pass that finds it and not the first, since a build makes thousands of processes that are gone before the next pass and reading each one costs the pod more than the kind it would give:
a process the sampler has not read a command line for carries the kind of its nearest ancestor that has one, and takes its own from the pass that reads it.
So the first two seconds of a command the agent started are charged to whatever started it, and a command that lived less than one pass is charged there whole.

A walk charges only the CPU it watched.
The first pass of a sampler's life records what `/proc` already holds for every process it finds — each process's own CPU and the CPU of everything it has reaped — as CPU already charged, so every `_cpu_seconds_total` of a role and kind pair starts at zero and rises with what the pod spends from then on.
A restart of the sampler is therefore a counter reset, which Prometheus reads correctly, and `legion_sampler_restarts_total` is where a reader sees that one happened.
Charging those counters instead would move the role's history to the wrong kind: nothing in them says which kind of work earned the seconds, and a role's reaped lifetime — every build and test its agent ever waited for — went to `tool` in one pass.
The pod's own `legion_sampler_pod_cpu_seconds_total` comes from its cgroup and keeps its history across a restart.

## The peak, and what a second reader does to it

A `_peak_bytes` series is the most its reading held since the last scrape, and writing the exposition resets every peak to the reading beside it.
That is what makes the peak answer "how much did this role hold between your two scrapes", which one reading a minute cannot.

The reset happens for whoever scrapes.
A second reader shortens the first reader's window:
read `/metrics` by hand while the collector is scraping every 30 seconds, and the collector's next peak covers only the seconds since your read, not the thirty it expected.
Read the port by hand while you are debugging, and know that the peaks of that scrape interval are then yours and not the dashboard's.

## The hour window and the drop order

A role and kind pair is served while a process of that pair ran within the last hour.
An hour after the last of its processes ran, the pair's series leave the output.
Prometheus reads a counter that stops and later returns correctly, so nothing is lost by this and the series set stays small.
The same window applies to a grade pod's kinds in node mode.

A pair whose processes have all ended holds no memory.
Each walk sets the memory of every pair it found no process of to zero, resident and anonymous alike, so the per-kind parts go on summing to the pod's totals, while the pair's CPU counter stands where it stopped and its series stay in the output for the rest of the hour.
The same holds for a grade pod's kinds in node mode.
The cgroup profile is the exception named below: there a command's last reading stands until the pair is dropped, because the cgroup is what is read and the launcher is asked to keep it one pass longer.

The sampler serves at most 200 series for one pod, and in node mode at most 200 for each grade pod.
When the set is over the cap, it drops in this order:

- the sampler's own series and the pod totals are never dropped;

- then role and kind pairs, the pair whose process ran longest ago first, each pair's series kept or dropped together;

- then volumes, by name.

`legion_sampler_series_dropped` is how many series the cap removed at that scrape.
An issue pod as the daemon builds it today is 171 series at its widest, so the cap binds only on a pod with more volumes than Legion declares.

## The exposition

The sampler writes the Prometheus text format itself, with no metrics library:
`Content-Type: text/plain; version=0.0.4; charset=utf-8`, one `# HELP` and one `# TYPE` line before each metric's series, the series of a metric sorted by their labels, and `\`, `"` and a newline escaped in a label value.
Two scrapes of one unchanged state are the same bytes.

## What node mode is deployed with

The DaemonSet asks for 50m of CPU and 64Mi of memory and is limited to 200m and 128Mi.
Those are the numbers dispatch://INFRA-1209's B3 deploys, and they hold the sampler's own cost rule:
3% of one CPU over any minute in which `legion_sampler_processes_seen_total` rises by at most 20 a second, and an exit of its own when its anonymous memory passes 64 MiB.
Above that rate the cost grows with the rate of new processes, since the walks and the memory totals are then already at their floors and nothing is left to give way.

## What a per-command cgroup must keep

Whoever adds the per-command cgroups of the cgroup profile must keep an ended command's cgroup until the sampler has read it once more.

The sampler reads each command's cgroup on a walk pass, which is every two seconds while the window's allowance can pay for it and never less often than once every ten seconds:
a cgroup kept for ten seconds past the command's end is read once more whatever the pod was doing, where two seconds is enough only on a pod whose sampler is deferring nothing.
Without that, nothing disappears from the output:
the sampler keeps a removed cgroup's last values in its own memory and goes on serving them until the hour window drops the pair.
But those are the values of the last read before the command ended, not the command's final ones, and a build that does most of its work in its last second is then reported as though it never did.
A kept cgroup gives the true final numbers.

## What the sampler never carries

No series carries a command line, a command argument or an environment value.
The sampler reads `/proc/<pid>/cmdline` to decide a kind and nothing else:
the one function that reads it answers a kind, so there is nothing for a label to be made of.

No series carries a `pod` label either.
Managed Prometheus adds the pod, the namespace, the node and the cluster from the scrape itself, and a `pod` label written here would collide with it.
