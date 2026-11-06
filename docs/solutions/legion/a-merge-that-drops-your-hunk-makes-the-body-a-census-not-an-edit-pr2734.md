---
title: "A merge that drops your hunk makes the pull request body a census, not an edit"
category: legion
tags:
  - forward-merge
  - pr-body
  - review
date: 2026-11-04
status: active
module: skills/legion-worker
related_issues:
  - "cybermonklabs/legion#2734"
---

# A merge that drops your hunk makes the pull request body a census, not an edit

Extends docs/solutions/legion/a-forward-merge-onto-mains-competing-rule-keeps-mains-code-and-deletes-your-copy-and-its-tests-pr2366.md.

That page says to rewrite every comment, doc line and changelog fragment against main as it now
is. The pull request body is not in its list, and the body is the one artifact another role
quotes.

- **Treat it as a census driven by the file set, not as an edit driven by memory.** Every path
  that left `gh pr diff <n> --name-only` across the merge is a noun: grep the body for it, and for
  what the dropped hunk claimed. The file-set comparison is already required beside the
  fingerprint, so this costs one more read of output you have.
- **Removing the obvious bullet is not the census.** A claim about a dropped change lives in
  `Change`, in `Outcome`, in `Why`, in a risk paragraph, and in the fields another role wrote —
  `Look at first`, `Not proven / risk`. Those are still claims about *your* diff, and the merger
  quotes `Outcome` in its READY packet.
- **Re-read the issue's acceptance criteria too.** One the merge now satisfies through the base
  rather than through your diff is still an acceptance criterion: say where it is met, or the
  architect accepts a claim the diff no longer carries.
- **For the reviewer of such a round: diff the two fingerprint inputs, not just the hashes.** All
  removals bounds what the round can have broken — but match each removed hunk to the line the
  base now carries. Here the removals *were* a fix; "removals only" is a bound on the reading, not
  a proof that behaviour is unchanged.

## Evidence

cybermonklabs/legion#2734 carried a repository-id fix to two CI files. Main landed the same fix in
#2740 four hours later, and the pull request's second forward merge dropped its own version,
taking main's lines whole.

- The implementer removed the `Change` bullet and the "stays unproven" paragraph, and **left the
  claim in `Outcome`, in `Why` and in `Look at first`**. The reviewer found all three in its
  round-4 review and edited them out, recording it as the round's one finding and noting that the
  merger quotes `Outcome`.
- The file set went 239 → 229 at that merge, and the ten paths that left — the `ci-ok` action, now
  byte-identical to main, and nine generated copies reverted to main — were each the subject of a
  body sentence.
- The reviewer accepted the changed fingerprint
  (`f14e89ec…` → `ab6e5cdb…`) by diffing the two hash inputs line by line and finding that the new
  head **adds no line**, then naming every departure from the file set. That is what let a changed
  fingerprint be approved in one round rather than re-argued.
