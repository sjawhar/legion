---
title: "A proof pastes every commit SHA and resolves it before the handoff"
category: legion
tags:
  - proof
  - pr-body
  - handoff
  - commit-sha
date: 2026-10-25
status: active
module: skills/legion-worker, .legion handoffs
related_issues:
  - "INFRA-232"
  - "sjawhar/legion#2195"
---

# A proof pastes every commit SHA and resolves it before the handoff

Extends docs/solutions/legion/a-proof-quotes-the-runners-summary-line-verbatim-and-carries-no-total-across-rounds-LEGION-634.md.

That document covers counts typed from memory. A commit SHA is the same kind of claim, and it is
easier to get wrong, because a correct short prefix makes a wrong full SHA look right.

- **Never type a full SHA.** Put it in a shell variable from `jj log -r <rev> --no-graph -T
  commit_id`, or paste it from `gh` output, and write the text from that variable. This covers
  `headSha`, the PR body's `E2E` and `Threads` lines, a blob link, and a review-thread reply.
- **Before each handoff push and each body edit, resolve every full SHA you wrote.** Extract them
  with `grep -oE '[0-9a-f]{40}'` and run `gh api repos/<owner>/<repo>/commits/<sha>` for each.
  Any 422 is a defect to fix before you push. Also fetch each blob link and look for HTTP 200.
- **A short SHA in prose is safe only when you copied it.** Do not lengthen a 12-character
  prefix from memory.

## Evidence

- sjawhar/legion#2195, round 1: `implement.json` (A1, A6, A7), the `E2E (implementer)` line and
  its blob link named `81e2fa83374f00f9b2fd7d0e1bae39a1f15c50c7`. The real commit is
  `81e2fa83374f515a0badcbbc5f6133ac22129e40`: the first twelve characters were right and the rest
  was invented. GitHub answered 422 and the link 404, and the tester failed the round on that
  alone, though every run it repeated matched. The architect noted that a reader cannot tell a
  transposition from a fabrication.
- Round 3: the first reply on a review thread named the head `4a9715060e1bae…`, again typed. The
  head was `4a9715060e1bbafef8754032794ef742fa421217`. The reply was edited, and the resolve check
  above was then run over the body and the handoff before each push.
