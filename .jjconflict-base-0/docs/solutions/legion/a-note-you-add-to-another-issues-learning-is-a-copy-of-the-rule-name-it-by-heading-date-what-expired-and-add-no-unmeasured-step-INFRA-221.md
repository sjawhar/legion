---
title: "A note you add to another issue's learning is a copy of the rule: name it by heading, date what expired, and add no unmeasured step"
category: legion
tags:
  - docs-solutions
  - legion-worker
  - review-round
  - pointer
  - supersede
date: 2026-10-25
status: active
module: docs/solutions
related_issues:
  - "INFRA-221"
  - "sjawhar/legion#2149"
---

# A note you add to another issue's learning is a copy of the rule: name it by heading, date what expired, and add no unmeasured step

Extends docs/solutions/legion/a-contract-numbers-prose-is-declaration-history-or-current-write-current-as-a-pointer-and-sweep-docs-solutions-too-LEGION-578.md.

That note says to write a current value as a pointer and to include `docs/solutions/` in the sweep.
The delta: a dated note written into a standing learning during implementation (a `**Note <date>
(<KEY>):**` or `**Superseded <date> (<KEY>):**` line) is a copy of your change's rule too. It drifts
the way any copy does.

- **Name the rule by its heading, and keep only what is local to that learning.** A one-line
  paraphrase keeps one condition of a two-condition rule and drops the other.
- **Before you write "still the step", read the learning's own premise against the tree as it is
  now.** A premise can be true when its learning merged and false hours later. Date the expiry from
  the two merges (`gh pr view <n> --json mergedAt`) and the line that changed it
  (`jj file annotate`), not from memory.
- **Make no endorsement of a placement or step your own rule refuses.** If your rule fails the
  learning's recipe, say so and name what replaces it.
- **Add no instruction nobody measured.** A note annotates another issue's recipe. It does not
  change the recipe's steps. Keep a scope claim the tester measured, and leave out an example
  command nobody ran against that recipe.
- **Before the push, read every note you wrote as a reader following its pointer.** Ask whether
  each claim is still true of the tree. A search for phrasings does not answer that.

## Evidence

sjawhar/legion#2149 annotated five active learnings (LEGION-695, LEGION-748, INFRA-42, INFRA-43 and
INFRA-44). Three test and review rounds in a row then found defects in those notes alone:

- **Review round 8:** the LEGION-695 note called a Postgres built under `/tmp` "still the step",
  which fails the first condition of the rule it cited. The INFRA-42, INFRA-43 and INFRA-44 notes
  stated the rule by its second condition alone.
- **Tester round 10:** the LEGION-748 note still said "That line is about scope" and vouched for
  LEGION-695 as "a harness that passes the test". The INFRA-44 note re-endorsed a bullet whose
  premise, "No role container sets `TMPDIR`", was true when sjawhar/legion#2088 merged
  (2026-10-21 03:32Z) and false from sjawhar/legion#2085 (06:29Z). That change added `TMPDIR`,
  `GOCACHE` and `GOMODCACHE` to `roleEnvironment` in
  `packages/daemon/internal/runtime/sandbox/manifest.go`.
- **The last read:** an end-to-end read of all five notes found a sixth claim that was no longer
  true (INFRA-43's sourcing). Its fix first added a subshell example nobody had run against the
  harness. The architect ruled that example out and kept the measured scope clause: the env file
  goes to the harness's commands, "not into the shell you go on working in".
