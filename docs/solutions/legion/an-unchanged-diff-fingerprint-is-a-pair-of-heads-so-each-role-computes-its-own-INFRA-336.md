---
title: "An unchanged-diff fingerprint is a verdict about a pair of heads, so each role computes its own"
category: legion
tags:
  - fingerprint
  - forward-merge
  - test-rounds
  - verification
date: 2026-10-31
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-336"
  - "sjawhar/legion#2585"
---

# An unchanged-diff fingerprint is a verdict about a pair of heads, so each role computes its own

Extends docs/solutions/legion/unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md, which
gives the command and its fileset. This is about whose two heads go into it.

- **"Unchanged" is a property of two heads, never of a push.** The same forward merge is
  unchanged for one role and changed for another, and both verdicts are right.

- **Each role recomputes from the head *it* last verified**, which is what the skill's table
  means by naming a different pair per role: the implementer its own tip before and after its
  merge, the tester the head its `E2E` line names, the reviewer the head of its last submitted
  review. Never adopt another role's verdict, however recent its comment.

- **A fingerprint comment names both endpoints and whose they are**, so the next role can see at
  once that the pair is not its own.

- **The cost of taking someone else's verdict is the round it saves.** An "unchanged" borrowed
  from a role whose window began later buys bare gates over a span in which product text was
  rewritten.

## Evidence

In INFRA-336, the implementer's forward merge of `main` left the branch's own lines untouched:
its comment on the pull request records `ec7c7ad0 → 4389e477`, fingerprint
`a19fe705…` → `a19fe705…`, unchanged. That is correct for its own pair, the tip before and after
its merge.

The tester's handoff for the same head records a different pair and the opposite verdict:
`cdd83c7d → 4389e477`, `05772da3…` → `a19fe705…`, **changed — a full test round, not bare gates**.
Its reason: between the head that tester last verified and the merge tip sat the reviewer's prose
commit `dc17476b`, which rewrote the handoff sentence in `go/worker-common.md`, in all four role
residues and in `skills/legion-worker/SKILL.md` — product text of the change that no tester had
read.

Both numbers are right. Only the tester's answers the question the tester had to ask, and it was
reachable only by recomputing from its own last verified head.
