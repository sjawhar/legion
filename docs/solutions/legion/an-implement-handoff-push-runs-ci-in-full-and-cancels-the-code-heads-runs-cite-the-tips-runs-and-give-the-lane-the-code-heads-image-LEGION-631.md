---
title: "An implement handoff push runs CI in full and cancels the code head's runs: cite the tip's runs, and give the lane the code head's image"
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
# An implement handoff push runs CI in full and cancels the code head's runs: cite the tip's runs, and give the lane the code head's image

- Only three handoff pushes skip CI (the planner's `plan.json`, the tester's `test.json`, a
  reviewer's `changes_requested` `review.json`): their phase guarantees a later push. An
  implementer's `implement.json` push runs every workflow in full, and `pr-and-main.yaml`'s
  concurrency group (`tests-<ref>`, `cancel-in-progress` on `pull_request`) cancels the code head's
  `Tests`, `Docs` and `Legion Envoy and Contracts` runs still in flight. The green runs for that
  code are the handoff head's; they stand for the code head only because the handoff commit's
  diff over it is `.legion/` alone — say so in the `CI` line, naming both heads and the cancelled
  run ids, so a reader does not take "cancelled at the code head" for a red.
- Push the implement handoff right after the code push rather than after watching the code head's
  CI: one run then covers both commits, and nothing is cancelled half-way.
- `Worker Image` has no such group: it completes at both heads, so one logical code change has two
  digests (`sha-<code head>` and `sha-<handoff head>`, the same build inputs but for the revision
  label). The operator lane runs the code head's image; give the coordinator the code head, its
  digest, and the handoff head's green run ids in one message, and the tip's digest beside them in
  case its scripts hold the image's revision against the branch tip.

## Evidence

sjawhar/legion#1843, round 9: the merge commit 38293495's `Tests` 38042284670, `Docs` 38042284637
and `Legion Envoy and Contracts` 38042284625 read `cancelled` once the handoff push 72e7b053 arrived
(its `Tests` 38042422509, `Docs` 38042422519 and `Worker Image` 38042422505 green); both `Worker
Image` runs completed (`sha-382934956bf3@sha256:4368aa92…`, `sha-72e7b053fd06@sha256:c7beda88…`);
the coordinator ran Stage 4a and 4b on the code head's `4368aa92…` and asked for the tip's digest
too. The tester's round 8 confirmed the handoff commit's diff over the merge is `.legion/` alone
before taking 72e7b053's runs as the merge's CI (`.legion/LEGION-631/test.json`, round 8).
