---
title: "A changelog entry is a delta over the release its reader meets, and the fold rule decides which release that is"
category: best-practices
tags:
  - changelog
  - changie
  - fold
  - release
  - concurrent-pull-requests
date: 2026-11-03
status: active
module: packages/pi-envoy
related_issues:
  - "sjawhar/legion#2699"
---

# A changelog entry is a delta over the release its reader meets, and the fold rule decides which release that is

Extends docs/solutions/best-practices/a-sentence-you-keep-in-a-merge-is-a-claim-about-the-other-sides-code-pr2542.md,
which says a sentence kept through a merge is a claim about the other side's code. A changelog
entry makes that claim about a released artefact, and it can go false with no line of your code
changing.

- Find which release will contain the commit that adds your fragment. The fold labels a fragment
  with the earliest tag containing that commit, so a sibling fragment added earlier folds under an
  earlier release and yours describes the world after it.
- Write your entry against that release's code, not against the branch you cut from. A fix another
  tree shipped in the meantime is already the reader's starting point.
- Repeat no sentence of a sibling fragment that folds under an earlier release: one release would
  otherwise carry two entries for one fix, and the older one is the one a reader believes.
- Leave the sibling fragment alone when it is true of its own release. Correcting history is not
  the fix; writing a true delta is.
- Check each claim against the released file rather than reading your entry for sense. Read the
  package's source at the commit the tag contains, and measure the defect you claim where you can.
  A claim about a release is as checkable as a claim about a line.

## Evidence

sjawhar/legion#2699 (INFRA-436) carried a fragment written before `main` merged sjawhar/legion#2625
— the same fix, by another tree, in the shape this branch had at its first round. Tag
`pi-envoy-v16.0.0` contains #2625's commit, so #2625's fragment folds under 16.0.0 and this one
under the release after it, where three of its claims were false of what the reader has: 16.0.0
*refused* the registration while the agent subject was unanswered rather than waiting an outage
out, paid one bound per pass rather than one per subject, and confirmed a stranded subject on a
later pass. Each was refuted by a line of 16.0.0's own `envoy.ts`, read at that commit.

The rewrite names 16.0.0's real defects instead — the refused registration, the failed open left
open while its caller unmarks it, and the relay subject that took no round trip at all — and shares
no sentence with the sibling fragment, compared sentence by sentence after writing. The tester
checked the premise by running 16.0.0's own extension and measuring it. Missing this cost two
review rounds on a pull request whose code was already approved.
