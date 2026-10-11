---
title: "A conflict round that changes the fingerprint is a counted review round, and its approval starts a second retro"
category: legion
tags:
  - forward-merge
  - unchanged-diff-fingerprint
  - review_round_cap
  - pr-blocked
  - retro
  - workflow-table
date: 2026-10-08
status: active
module: packages/daemon
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1846"
---

# A conflict round that changes the fingerprint is a counted review round, and its approval starts a second retro

- A forward merge of `main` whose unchanged-diff fingerprint changes is a full round
  (`skill://legion-worker/references/conflicts-and-rewrites.md`), and the daemon counts its
  `changes_requested` like any other: `recordRound` (`packages/daemon/internal/workflow/review.go`)
  increments the implementer's `Rounds` on every rejected round, and at `review_round_cap` (3 unless
  `legion.yaml` sets it) posts `Issue reached review_round_cap=N.` on the Dispatch issue and sends
  the architect a `pr-blocked` notice naming it. Nothing stops — the next round runs — but the
  architect is told, and will ask for the smallest round that closes the findings. Make a conflict
  round's fixes one commit, answer the blocking threads with it, and send non-blocking findings to
  the named fast-follow comment, answering their threads `Declined:` with its link.
- The workflow table runs retro after every approval (`table.go`: `Reviewing` + `TriggerReviewApproved`
  → `Retro`), so a conflict round's new approval starts the implementer on retro a second time. The
  retro skill's "a conflict-forced rebase after retro moves these documents with the branch; retro
  does not re-run" is the other case: a rebase that changes nothing the reviewer re-decides, where
  the approval stands and the tree goes to the merger. A second retro covers only what the merge,
  the renumber and the review rounds since the first retro taught; it writes new files and edits
  none of the first retro's, re-reads `mergeable` before committing
  (docs/solutions/legion/a-stopgap-that-lands-on-main-mid-plan-is-superseded-not-merged.md, item 7),
  and brings the body's `Size` and `Chain` lines up to date for its commit.
- A first retro's note that states a current number, a head or a "no release has shipped" claim can
  become a review finding in the next round, fixable only by the implementer in an implementing
  round (retro never edits `.legion/`, and a round-4 sweep had to edit two retro notes). Write such
  facts as history with their moment, or as pointers
  (docs/solutions/legion/a-contract-numbers-prose-is-declaration-history-or-current-write-current-as-a-pointer-and-sweep-docs-solutions-too-LEGION-578.md).

## Evidence

sjawhar/legion#1846 (root LEGION-578): rejected rounds 1 and 2, approval at 96a3949f (review
5454722232), the first retro's seven notes at b855a3d4. `main` then moved (LEGION-462's #1752, the
pi-legion 8.2.0/8.3.0 releases) and GitHub reported CONFLICTING; the forward merge 38e27ad3 plus the
test fix 7721ffa0 changed the fingerprint (`6f7ff644…` → `ec31f821…`, the tester's own computation
`6f7ff644…` → `7e9df573…`), so the tester ran a full round and the reviewer the thermonuclear pair
again. Round 4's `changes_requested` (five prose sentences at the old contract number) was the root's
third rejection: the architect's direction, 2026-10-08T22:47Z, read "the daemon reports the root at
its review round cap, so make this round one commit", and the round was f98f84e7 alone with the
review's three minors in a fast-follow comment (issuecomment-6070622880) and their thread answered
`Declined:`. The reviewer approved again at 5ffc69c5 (review 5463972815), and the daemon started
the implementer on retro a second time; this note and its three siblings are that retro's output,
above the approved head, with the first retro's seven files untouched.
