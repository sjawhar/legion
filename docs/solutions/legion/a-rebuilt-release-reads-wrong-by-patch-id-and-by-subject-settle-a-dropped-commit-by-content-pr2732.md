---
title: "A rebuilt release reads wrong by patch id and by subject: settle a dropped commit by content"
category: legion
tags:
  - dependency-bump
  - releases
  - evidence
  - git
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2732"
---

# A rebuilt release reads wrong by patch id and by subject: settle a dropped commit by content

Extends docs/solutions/legion/release-bound-claims-are-verified-against-tags-and-tarballs.md.

That note verifies a release claim against the tag and the published tarball. Tag ancestry answers "not contained" for a commit that **is** carried, whenever the release was rebuilt rather than fast-forwarded, so for a rebuilt release add this.

- **Neither cheap reading answers "is this change carried".** A subject diff under-counts, because a rebuild merges subjects. `git cherry` over-counts, because a squash changes every patch id. Expect the two to disagree by an order of magnitude, and treat both as questions rather than as counts.
- **Settle it by content**, commit by commit, with the upstream owner when the history is theirs. Until then write every figure as what it is: a reading of subjects, or a reading of patch ids, never a number of lost changes.
- **Record only the drops that have a consequence for this deployment, and spell the consequence out.** A list of absent subjects tells a later reader nothing it can act on; "a session in this state now does X instead of Y" does.
- **Say whether a drop is deliberate**, because that decides whether anyone waits. A deliberate drop is never recovered by a later release, so the pull request must not promise a recomposition that nobody will make.
- Do not carry the provisional reading forward once the content check lands. Replace it, so the record a reader finds is the settled one.

## Evidence

sjawhar/legion#2732 moved the Oh My Pi pin to a release that is not a fast-forward of the pin it replaced: `compare` answers `diverged`, `ahead_by` 1240 and `behind_by` 59, with 58 commits on the old tag's side alone.

The same drop was read three times, with three answers:

| reading | answer |
| --- | --- |
| commit subjects absent from the new release | 19 |
| `git cherry`, by patch id | 56 of the 58 |
| content, commit by commit, by the upstream owner | 2 |

55 of the 56 are carried in the release; the rebuild squashed them, so their patch ids changed and `git cherry` could not match them. Exactly two are absent, dropped deliberately on 2026-10-31 to match upstream's documented policy: the turn-recovery fix and its print and rpc-ui part. No later release brings them back.

Their consequence for this fleet, which is the only reason the pull request records them: a headless or RPC session that reaches the usage reserve, under the default `retry.usageReservePolicy=confirm`, falls back to the next model in its chain, and keeps its model when it has no fallback. It does not stall.

The pull-request body carried each reading in turn as it was made, and was rewritten when the content check landed, including the removal of a sentence promising that a later release would recompose the drop. That sentence was true of the hypothesis and false of the facts.
