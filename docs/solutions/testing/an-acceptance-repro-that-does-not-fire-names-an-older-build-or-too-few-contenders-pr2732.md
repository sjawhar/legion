---
title: "An acceptance repro that does not fire names an older build, or too few contenders"
category: testing
tags:
  - dependencies
  - regressions
  - evidence
  - reproduction
  - worker-pool
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2732"
---

# An acceptance repro that does not fire names an older build, or too few contenders

Extends docs/solutions/testing/attribute-a-dependencys-behaviour-to-a-release-by-one-probe-on-a-real-artefact-at-each-release-pr2444.md.

An issue's acceptance line usually quotes a reproduction lifted from production transcripts. Run it against the exact build you are replacing, first thing. When it does not fire, two causes are far likelier than "the issue is wrong", and each has its own check.

- **The transcripts ran an older build than the one you replace.** The build live on the transcripts' dates is a fact to look up, not to assume. Walk back to it, confirm the repro there, and publish **both** results: the negative on the build being replaced is evidence, not a result to drop. A repro that fires only on an already-replaced build is not acceptance evidence for this bump, and saying so costs less than widening the repro for ever.
- **The repro's count is a property of the host, not a number in the prose.** When the defect starves a runtime's worker pool, the number of triggers is the pool's size, which the runtime takes from the host's available parallelism. One fewer answers normally and reads as "does not reproduce". Start at the host's own count, read it at run time rather than hard-coding it, and record the count beside every timing so a reader on a larger host knows to raise it.
- **The process shape is part of the repro.** A busy loop inside a subshell is a different shape from the same loop in the shell the tool runs, and only one of them starves the pool. Vary the shape before you conclude the trigger is wrong.
- Write the driver so its steps come from the command line. Then a tester on another host raises the count without editing it, and the shapes you ruled out are recorded as the commands that ruled them out.

## Evidence

sjawhar/legion#2732 raised the Oh My Pi pin for a wedge whose acceptance line read: "In a pod on the new pin: `mapfile -t x < <(printf a)` twice, and a background busy loop, each return, and a `grep` afterwards answers."

On the build being replaced, `omp/18.13.3`, in a two-CPU pod, driven through a child `omp --mode rpc` on `packages/pi-shared/test/omp-harness.ts`:

- `mapfile -t x < <(printf a)` answered at every count tried, up to eight calls in one session, with the `grep` tool and a `bash` after them answering.
- `( while :; do :; done ) &`, the loop inside a subshell, did not wedge it either, with two `mapfile` calls before it.
- One bare in-shell `while :; do :; done &` answered in 1411 ms and left the session answering.
- **Two** bare in-shell loops wedged it: the second never answered within 60,004 ms. On `omp/18.16.0` all of these answered, the second loop in 312 ms.

Two is this pod's `os.availableParallelism`. The tester re-ran the same driver with four loops and the two builds still parted, which is the same rule applied with headroom.

The mapfile half was then attributed to its own build. The four wedged transcripts are dated 2026-10-25 to 10-29, and main pinned `omp/18.12.2-sami.20261026-135627` on 10-26. On that build a **single** `mapfile -t x < <(printf a)` gave no answer within 60,002 ms. So the mapfile wedge was fixed between 18.12.2 and 18.13.3, the busy-loop wedge survived into 18.13.3, and the release fixes both. Had the round stopped at "the acceptance line does not reproduce", the issue would have read as unfounded.
