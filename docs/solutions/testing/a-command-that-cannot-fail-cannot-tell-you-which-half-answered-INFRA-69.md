---
title: "A command that cannot fail cannot tell you which half answered: run the instrument bare, and confirm an absence two ways when a fallback reported it"
category: testing
tags:
  - measurement
  - issue-pod
  - cgroup
  - shell
  - evidence
date: 2026-10-21
status: active
module: docs/solutions
related_issues:
  - "INFRA-69"
  - "sjawhar/legion#2088"
---

# A command that cannot fail cannot tell you which half answered

`cmd_a 2>/dev/null || cmd_b` prints a number and hides which command produced it. Write the number
down beside `cmd_a`'s path and you have published a real measurement wearing a path a reader cannot
open.

- **Run the instrument bare when you are going to quote it.** No `2>/dev/null`, no `||`. If it
  fails, that is the finding.
- **`ls` or `find` the path before you write it into a sentence.** A path quoted from the first
  half of a fallback is a guess until something has looked for it.
- **When a fallback is what reported an absence, confirm the absence two ways.** The thing that
  hid it once will hide it again.
- **The same applies to a path, flag or file taken from another role's message.** Their
  environment is not necessarily yours, and a reading you did not take is not a reproduction.

## Evidence

In an INFRA-69 role container the command was
`cat /sys/fs/cgroup/memory.current 2>/dev/null || cat /sys/fs/cgroup/memory/memory.usage_in_bytes`.
The fallback fired silently, and the figures `766,550,016 → 871,407,616` — correct, taken from the
v1 counter — were written into a `docs/solutions/` note beside the v2 path. The sandbox is cgroup
v1: `ls /sys/fs/cgroup` lists `cpu cpuacct cpuset devices job memory pids`,
`ls -l /sys/fs/cgroup/memory.current` answers *No such file or directory*, and
`find /sys/fs/cgroup -maxdepth 3 -name memory.current` prints nothing. The tester found it by
running those three; the fix confirmed the absence with both `ls` and `find`, because one silent
fallback had already been enough.

The path itself had been adopted from a reviewer's finding, which had taken its own reading the
same way, and a thread reply then reported it as an independent reproduction. Three roles carried
one unopenable path because none of them had looked for it.

A second instrument defect in the same note has the same shape from the other direction: `du -sx /`
was unchanged across a 200 MiB write, and the note concluded `du` could not see the file. `du`
pointed at the path reports it exactly (`du -sk /tmp/probe` → `204800`); what is blind is the
one-filesystem walk, because on this gVisor overlay a written file reports a different device from
its parent directory. A measurement that surprises you is a reason to run the neighbouring command,
not to generalise from the one you ran.
