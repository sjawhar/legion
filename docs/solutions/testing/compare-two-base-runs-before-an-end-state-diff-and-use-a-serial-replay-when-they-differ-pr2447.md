---
title: "Compare two base runs before an end-state diff, and use a serial replay when they differ"
category: testing
tags:
  - load-test
  - replay
  - equivalence
  - dispatch
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2447"
---

# Compare two base runs before an end-state diff, and use a serial replay when they differ

When you must show that a change leaves a replayed load's end state unchanged:

- First compare two base runs. If they differ, a base-against-head difference proves nothing.
- If they differ, find the source. A replay whose worker threads share one random generator makes
  its choices in thread-timing order. Two runs of one build then pick different quotes and edit
  different text.
- Then run a serial copy of the tool: one worker, no pacing, every event in log order. Run it twice
  at base and once at head, each from a fresh restore. Use the concurrent tool only for the CPU
  measurement.
- Dump the end state in an unambiguous format. Text with newlines or tabs breaks a TSV dump, so use
  one JSON array per row, and compare rows as multisets. A dump in which `ORDER BY 1::text` sorts by
  a constant gives no line order, and a line diff then shows false differences.
- In a harness script, stop servers with `pkill -x <binary name>`. `pkill -f <path>` also matches
  the script itself when its own arguments hold that path, and it then kills the whole run.

## Evidence

- In sjawhar/legion#2447 (implement.json, AC2c), the Dispatch CPU replay's `tools/load.py` shares one
  `random.Random` across 96 threads. Two concurrent base runs differed on 84 of 1,165 latest
  versions, so the plan's check ("any difference must also appear base vs base") could not decide.
- A serial copy (`load_serial.py`: `ThreadPoolExecutor(1)`, no sleeps, `cf.wait(futures)`) gave
  identical results across sbase1, sbase2 and shead2: 3,476 anchor rows as multisets, and the md5 of
  all 1,165 latest versions.
- The first TSV dump split multi-line comment bodies into extra rows. The JSON dump with
  `order by 1::text` showed 70–82 differing lines with zero differing rows.
- The first batch of six runs ended `Terminated` at once: `pkill -f /…/bin/dispatch-` matched
  `run1.sh /…/bin/dispatch-base`.
