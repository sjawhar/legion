---
title: "A seed template keeps no row a sweep acts on, and a seed step probes its tools before it drops the old seed"
category: testing
tags:
  - load-test
  - seed
  - template-database
  - idempotency-keys
  - measurement
date: 2026-10-29
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2469"
---

# A seed template keeps no row a sweep acts on, and a seed step probes its tools before it drops the old seed

Extends docs/solutions/best-practices/measure-a-cpu-fix-with-full-pprof-focus-totals-a-per-line-split-and-a-restore-past-boot-backfills-pr2416.md.

That document waits out a backlog that runs at boot. This one covers rows the seed itself leaves,
which wake a background job only when the seed is old enough.

- **Delete what the seed's own writes leave for a background job, before the template is kept,
  and check that none is left.** List every table the server sweeps by age or by count. For
  Dispatch this includes `request_keys`, one row per write sent with an `Idempotency-Key`, which a
  sweep deletes after 24 hours. A template that keeps those rows gives every step on a seed older
  than the sweep's age extra work inside its window. A step on a younger seed does not get that
  work, so the same build measures differently depending on the seed's age. Delete the rows in
  the seed step, before the `vacuum`, and fail the step if any row remains.
- **Probe each tool the seed step needs before the step drops anything.** Run the subcommand's own
  `-h` and read its exit status (`loadgen seed-refs -h >/dev/null 2>&1 || die …`). Do not grep its
  usage text: a reworded help line then makes the probe report a missing tool. Put the probe above
  the `drop database` lines. A tool that is missing after the drop leaves no seed at all.
- **Key every write a seeding program sends through a retrying client**, one deterministic key per
  planned write, so a retry after a lost answer replays instead of writing twice. Prove it by
  running the program a second time on the seeded database and checking that the counts do not
  change. A count of the program's own successful calls proves nothing: a replayed answer counts
  too.

## Evidence

- sjawhar/legion#2469 keyed every `loadgen seed-refs` write (`refs-t<n>-v<v>`, `refs-s-<key>`).
  The first template then held 19,781 `request_keys` rows under `agent-token`. Dispatch's sweep
  (`sweepRequestKeys`, `IdempotencyKeyTTL` 24 h, run every minute from Dispatch's start) would
  have deleted them about 60 s into every step on a seed more than a day old. Review round 4
  caught it. Since c281bc3c6d8e, `stack.sh`'s `cmd_seed` runs `delete from request_keys` before
  `vacuum analyze` and dies when the count is not 0. A fresh seed then held 0 rows, and the
  counts stayed 1,000 targets, 19,471 versions and 310 sources.
- The second `seed-refs` run on the seeded stack exited 0 in 61 s, and the counts were unchanged.
  The same write sent without a key added a version.
- The first guard grepped the usage for `'^  seed-refs '`, which made a two-space indent a
  contract. It became `seed-refs -h` at c281bc3c6d8e. That head's loadgen exits 0 on it and main's
  exits 2. With main's loadgen in place, `stack.sh seed --force` exits 1, and the template,
  `seed.json` and `seed.meta.json` all remain.
- Review round 4 also removed two count checks that compared the program's own counter of 201
  answers with the plan. They could never fail.
