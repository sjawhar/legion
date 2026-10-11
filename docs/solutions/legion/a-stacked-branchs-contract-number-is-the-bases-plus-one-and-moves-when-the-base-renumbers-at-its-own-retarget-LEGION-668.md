---
title: "A stacked branch's contract number is the base's plus one and moves when the base renumbers at its own retarget: plan the renumber into the retarget, not into the forward merges"
category: legion
tags:
  - daemon-api-contract
  - contract-version
  - stacked-pr
  - retarget
  - renumber
date: 2026-10-11
status: active
module: packages/daemon/internal/api
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
  - "sjawhar/legion#1843"
---

# A stacked branch's contract number is the base's plus one and moves when the base renumbers at its own retarget: plan the renumber into the retarget, not into the forward merges

Extends docs/solutions/legion/a-contract-numbers-prose-is-declaration-history-or-current-write-current-as-a-pointer-and-sweep-docs-solutions-too-LEGION-578.md.

- A branch stacked on another contract-bumping branch takes the base's number plus one, and that
  number is as provisional as the base's: the base renumbers when it retargets onto `main` (its own
  collision), landing on the number you took, and the release cut from its squash declares it. So
  "free on `main` and on the base" checked at each forward merge of the base's older heads proves
  nothing about the base's final head; the collision arrives with the base's landing.
- Put the renumber on the retarget checklist with its whole site set — `version.go`'s entry and the
  collision note, the golden fixture, `pi-legion`'s manifest, its CHANGELOG entry and the
  `AGENTS.md` number history, `docs/kubernetes.md`'s rollout sentence, the PR body's census rows and
  rollout sentences — and run the LEGION-578 sweep over the body too: the reviewer found four
  rollout sentences still naming the old number after the code moved.
- Write the collision note in `version.go` at the time, naming the release that declares the old
  number without your shapes (`pi-legion 8.10.1` here); the next stacked branch reads it to see
  the pattern is the rule, not an accident.

## Evidence (LEGION-668, sjawhar/legion#1878)

Stacked on `legion/LEGION-631` (#1843) at 18, the branch took 19; the plan recorded "contract 19 is
free on `main` (17) and on `legion/LEGION-631` (18) as of this head; re-check at each forward
merge". #1843 had renumbered three times in its own life (16, 17, 18, then 19 at its retarget,
`version.go`'s entry 19 says so) and squash-merged declaring 19 with `pi-legion` 8.10.1. The
retarget merge of `530e753d` renumbered this branch to 20 across the six declaration and history
sites; the reviewer's round-4 note asked for the PR body's four rollout sentences, which the
retarget's own sweep had missed.
