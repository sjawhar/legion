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
| `legion_sampler_series_dropped` | gauge | — | series the 200-series cap removed at the last scrape |
| `legion_sampler_smaps_deferred` | gauge | — | processes the `smaps` budget carried into the next window |

All four sources of `legion_sampler_read_errors_total` are served from the first scrape, at zero, so a rate over one of them answers on a sampler that has never failed a read.

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
The rules are in `packages/daemon/internal/sampler/rules.go`, and a deployment adds its own with the daemon configuration key `runtime.kubernetes.sampler_rules`.

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
2% of one CPU, and an exit of its own when its anonymous memory passes 64 MiB.

## What a per-command cgroup must keep

Whoever adds the per-command cgroups of the cgroup profile must keep an ended command's cgroup until the sampler has read it once more.

The sampler reads each command's cgroup at least every two seconds, so one cgroup kept for two seconds past the command's end is enough, and the launcher may remove it after the sampler's next pass.
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
