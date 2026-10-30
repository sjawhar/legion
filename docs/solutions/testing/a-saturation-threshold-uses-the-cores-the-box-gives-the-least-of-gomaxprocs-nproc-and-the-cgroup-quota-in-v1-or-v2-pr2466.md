---
title: "A saturation threshold uses the cores the box gives: the least of GOMAXPROCS, nproc and the cgroup quota, in cgroup v1 or v2"
category: testing
tags:
  - load-test
  - saturation
  - cgroup
  - gomaxprocs
  - legion-pod
date: 2026-10-30
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2466"
---

# A saturation threshold uses the cores the box gives: the least of GOMAXPROCS, nproc and the cgroup quota, in cgroup v1 or v2

- **Measure "the process is pegged" against the cores it can actually use**: the least of its
  `GOMAXPROCS`, the box's CPU count (`nproc`, `runtime.NumCPU()`), and the cgroup's CPU quota when
  one is set. A threshold from `GOMAXPROCS` alone can be above what the box delivers, and then a
  pegged process never reads as saturated.
- **Read both cgroup hierarchies.** Try v2 first: `/sys/fs/cgroup/cpu.max` holds `<quota> <period>`,
  and `max` means no quota. When that file is absent, read v1:
  `/sys/fs/cgroup/cpu/cpu.cfs_quota_us` and `cpu.cfs_period_us`, in the `cpu` controller's own
  directory, not beside `cpu.max`. A v1 quota of `-1` means no quota: skip it; never take it as a
  number, or the least becomes negative and every run reads as saturated.
- **Record the figure used and which limit set it** (here `saturation_cores` and
  `saturation_cores_from`: `gomaxprocs`, `nproc`, `cgroup_cpu_max` or `cgroup_cpu_quota`), and record
  what was read from the cgroup files, so a reader can tell a v1 box from a box with no quota.
- **Check the hierarchy on the real box before trusting a reader of it.** Legion issue pods run
  cgroup v1: `cpu.max` does not exist there. A reader tested only against v2 fixtures returns "no
  quota" on every pod and looks correct.

## Evidence

`packages/envoy/loadtest/replay_record.go:303` (`readCgroupCPUQuota`) and `:330`
(`saturationCores`) are the reader and the least; `TestReadCgroupCPUQuota` covers the v2 file, v2
`max`, the v1 files, a v1 `-1` and neither file.

The first version tested `0.9 × GOMAXPROCS`. Runs at `--gomaxprocs 3` on a 2-core pod then had a
threshold of 2.7 cores, which the pod could not reach. The second version read only v2's
`cpu.max`; the reviewer read the pod itself (no `cpu.max`; `cpu/cpu.cfs_quota_us` 300000,
`cpu.cfs_period_us` 100000; `nproc` 2) and sent it back. Each cost a review round. A run on that pod
at `GOMAXPROCS` 3 now records the v1 quota 300000/100000 and `saturation_cores` 2 from `nproc`.
