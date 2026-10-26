---
title: "An implement handoff push runs CI in full and cancels the code head's runs: cite the tip's runs, and give the lane the image that stands for the code head"
category: legion
tags:
  - ci
  - concurrency
  - handoff
  - skip-checks
  - worker-image
  - operator-lane
date: 2026-10-10
status: active
module: .github/workflows
related_issues:
  - "LEGION-631"
  - "sjawhar/legion#1843"
---
# An implement handoff push runs CI in full and cancels the code head's runs: cite the tip's runs, and give the lane the image that stands for the code head

- The push rule since sjawhar/legion#1843: every push that changes only `.legion/` carries
  `skip-checks: true` and starts no workflow; a push that carries code and a handoff together, or a
  handoff commit pushed without the trailer, carries no trailer and runs CI at its head, which is a
  handoff commit (as sjawhar/legion#1944's plan commit 3b5c8f78 did on 2026-10-14: 33 check runs
  at a `plan.json`-only commit). Such a push runs every workflow in full, and `pr-and-main.yaml`'s
  concurrency group (`tests-<ref>`, `cancel-in-progress` on `pull_request`) cancels the code head's
  `Tests`, `Docs` and `Legion Envoy and Contracts` runs still in flight. The green runs for that
  code are the handoff head's; they stand for the code head only because the handoff commit's
  diff over it is `.legion/` alone — say so in the `CI` line, naming both heads and the cancelled
  run ids, so a reader does not take "cancelled at the code head" for a red.

  **Superseded 2026-10-23 (dispatch://INFRA-83):** only the clause that has you say so in the `CI`
  line, naming both heads and the cancelled run ids, is gone: the PR body has no `CI:` line
  (`skills/legion-worker/references/pr-body.md`), and no role records run ids for a later step to
  trust — the tester and the reviewer each read CI on GitHub at the head they are judging and record
  none of it (the reviewer's own four GitHub reads are in
  `skills/legion-worker/references/merge-gate.md`), where a cancelled run at the code head is visible
  beside the handoff head's green ones. The rest of this finding stands: the `skip-checks: true` push
  rule, the concurrency group cancelling the code head's runs, that the handoff head's green runs
  stand for the code head only because the handoff commit's diff over it is `.legion/` alone, and
  the `Worker Image` digests below as the 2026-10-26 note there amends them.

- Push the implement handoff right after the code push rather than after watching the code head's
  CI: one run then covers both commits, and nothing is cancelled half-way.
- `Worker Image` had no such group until 2026-10-26 (see the note below): it completed at both
  heads, so one logical code change had two digests (`sha-<code head>` and `sha-<handoff head>`,
  the same build inputs but for the revision
  label). The operator lane runs the code head's image; give the coordinator the code head, its
  digest, and the handoff head's green run ids in one message, and the tip's digest beside them in
  case its scripts hold the image's revision against the branch tip.

  **Superseded 2026-10-26:** `Worker Image` now has a per-PR group with `cancel-in-progress` on
  `pull_request`. A handoff push without the trailer cancels the code head's image build if that
  build has not finished, so there may be only the tip's digest (the same build inputs except
  for the image's revision label and the revision linked into its `legion version`, which names
  the tip). Give the coordinator the tip's digest and say that it stands for the code head.
  Anyone who needs the code head's own digest pushes the handoff only after the code
  head's image run has completed. A `.legion/`-only push that carries `skip-checks: true` starts
  no run and cancels nothing.

## Evidence

sjawhar/legion#1843, round 9: the merge commit 38293495's `Tests` 38042284670, `Docs` 38042284637
and `Legion Envoy and Contracts` 38042284625 read `cancelled` once the handoff push 72e7b053 arrived
(its `Tests` 38042422509, `Docs` 38042422519 and `Worker Image` 38042422505 green); both `Worker
Image` runs completed (`sha-382934956bf3@sha256:4368aa92…`, `sha-72e7b053fd06@sha256:c7beda88…`);
the coordinator ran Stage 4a and 4b on the code head's `4368aa92…` and asked for the tip's digest
too. The tester's round 8 confirmed the handoff commit's diff over the merge is `.legion/` alone
before taking 72e7b053's runs as the merge's CI (`.legion/LEGION-631/test.json`, round 8).

sjawhar/legion#1944, round 4: the plan commit 3b5c8f78, a `.legion/LEGION-573/plan.json`-only
commit pushed without the trailer on 2026-10-14, has 33 check runs: CI runs in full at a handoff
head whenever the push carries no `skip-checks: true`.
