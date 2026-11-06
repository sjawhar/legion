---
title: "A forward merge where main shipped an earlier draft of your own fix keeps yours and proves it by reconstruction"
category: legion
tags:
  - forward-merge
  - conflicts
  - supersession
  - fingerprint
  - implementer
date: 2026-11-03
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2699"
---

# A forward merge where main shipped an earlier draft of your own fix keeps yours and proves it by reconstruction

Extends docs/solutions/legion/a-forward-merge-onto-mains-competing-rule-keeps-mains-code-and-deletes-your-copy-and-its-tests-pr2366.md,
which covers the decision the other way: main's rule wins and your copy goes. This is the case
where what main merged is an earlier draft of your own change — the shape your review rounds
already corrected — so keeping main's would put the measured defects back.

- Ask the architect which side ships. It is a scope decision, not a how-to: it changes what lands
  over another tree's merged pull request. Resolve in the meantime rather than leaving the branch
  conflicting, and say in the report which way you resolved and how to reverse it.
- Resolve whole files, not conflict regions. The other draft's machinery reaches outside the
  regions jj marks — its own helpers, its callers, its registration filters — so a region-by-region
  resolution leaves half of each design, and it compiles.
- List the base's other commits that touch each file you took whole
  (`jj log -r 'fork_point(main@origin | <branch>)..main@origin & files("<path>")'`) and re-apply
  each by hand. That list is also the proof you lost nothing: anything not in it cannot be missing.
- Prove the result by reconstruction: your approved head plus those commits is byte-identical to
  the merge head, no symbol of the superseded draft is left in the tree, and every test row that
  draft deleted is accounted for — obsolete under the new shape, dropped on purpose, or still
  covered by a row of yours.
- Expect the fingerprint to change and a full test round to follow, and re-run the delivery proof
  at the merge head.
- Write the replacement into the pull request body as a fix of main: name the merged pull request
  and the time it landed, then each defect of the shipped shape with the review round that
  measured it and the row that fails without the fix. A reader who meets only the diff reads a
  revert.
- Read every changelog fragment of yours again afterwards
  (docs/solutions/best-practices/a-changelog-entry-is-a-delta-over-the-release-its-reader-meets-pr2699.md).

## Evidence

sjawhar/legion#2699 (INFRA-436) was returned CONFLICTING at its approved head: `main` had merged
sjawhar/legion#2625, the same cause fixed by another tree in the shape #2699 carried at round 1
(an `unconfirmed` map, a `confirmPending` pass, the bound paid per open). The architect decided to
keep #2699's shape, since #2625's carries the defects rounds 1 to 5 measured.

The merge took this branch's `envoy.ts` and `envoy.test.ts` whole and re-applied main's three other
commits touching them (a comment rewrite, an AskGate store beat with its row, and hoisted test
helpers with three de-flaked rows). The reviewer reproduced the head byte for byte from the
approved head plus those three commits, found no `#2625` symbol left under `packages/`, and
accounted for each of the 8 rows that draft deleted. Two `pi-legion` files left the pull request's
diff because main already carried their one changed line.
