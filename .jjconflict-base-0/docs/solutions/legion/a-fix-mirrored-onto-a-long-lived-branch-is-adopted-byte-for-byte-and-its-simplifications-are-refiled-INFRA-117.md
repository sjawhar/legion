---
title: "A fix mirrored onto a long-lived branch is adopted byte for byte, and every simplification found on it is re-filed against the main line"
category: legion
tags:
  - long-lived-branch
  - jj
  - adoption
  - ce-simplify-code
  - equivalence
date: 2026-10-22
status: superseded by AGENTS.md "Deploys come only from main" — no deployment renders from a long-lived branch, so no fix is mirrored onto one; the jj mechanics (duplicate keeps author and message, describe stales the printed id) stand
module: legion-worker
related_issues:
  - "INFRA-117"
  - "sjawhar/legion#2104"
  - "sjawhar/legion#2102"
---

# A fix mirrored onto a long-lived branch is adopted byte for byte, and every simplification found on it is re-filed against the main line

When a deployment rendered from a long-lived branch rather than the default branch (it no longer
may: `AGENTS.md`, "Deploys come only from main"), the same fix had to exist on both lines. On the
mirroring branch:

- **Adopt the existing commit; never retype the change.** `jj -R "$LEGION_WORKSPACE" duplicate
  <sha> -d <chain tip>` keeps the original author and message (the mechanic
  `docs/solutions/legion/stacked-base-and-review-folded-into-rebase.md` already names); a
  `jj describe` to credit the source leaves your role as committer. The adopted commit carrying a
  human author is correct and is not a tampering signal; say so in the handoff so no later role
  reads it as one.
- **`jj describe` makes the id `jj duplicate` printed stale.** Using that printed id in a later
  `jj new` resurrects the obsolete copy and leaves the change id divergent
  (`Change ID … is divergent`). Recover with `jj new <described commit>` then
  `jj abandon <stale commit>`, and check `jj log -r 'change_id(<prefix>)'` prints one commit.
- **Prove the tree, with a fileset.** The adoption sits on the issue's handoff commits, so the
  equality check is `jj diff --from <source sha> --to <adopted sha> --git '~.legion'` printing
  nothing. Without the fileset the handoffs read as a difference and the check looks failed.
- **Equivalence is the fix's hunks, not the file lists.** Two lines that have diverged by hundreds
  of commits cannot match file for file. Name the hunks in the plan (here: `usesCoreTransport`,
  `streamCaptures`, the permission constructor's publish list, the API's prefix refusal), extract
  each function from both heads and `diff` them. A difference inside one of those functions that
  is pre-existing branch shape (a helper the branch factored out) is not a divergence; say which
  it is.
- **Do not improve the mirrored hunks locally.** `skill://ce-simplify-code` and both review
  personas will find the same simplifications on an adopted fix. Applying any of them only here
  diverges the branch from the fix it exists to carry and adds a conflict to the eventual merge of
  the two lines. Record each finding with its reason for being skipped and re-file it against the
  main-line issue; the architect decides whether that line takes it.
- **Documentation and client text that the fix makes false belongs to the main line too**, for the
  same reason: every file edited on both lines is another conflict when the branches merge.

## Evidence

INFRA-117 carried the Envoy ad-hoc-topic fix onto `envoy-infra-integration`, the branch the Envoy
hub's permission plugin renders from (`diverged`, 290 ahead of and 208 behind `main` on
2026-10-22), by adopting `b096a8708` from the unreviewed #2102 rather than rewriting it.
`jj diff --from b096a8708 --to 7424e75ad6a1 --git '~.legion'` printed nothing; the twelve files
were that commit's tree exactly. `jj duplicate` printed `57e2c5f8`, `jj describe` rewrote it to
`7424e75a`, and `jj new 57e2c5f8` made the change id divergent until `jj abandon 57e2c5f8`.

Equivalence against the main-line pull request #2101 (`887cd0e2`) was read by extracting the four
functions from both trees: three identical, and the permission constructor's publish list
identical line for line, differing only in the KV-bucket loop, which this branch had already
factored into `bucketPermissions()` before the fix. The branch `legion/INFRA-113` did not carry
the fix at that time, which is why #2101 was the comparison.

`ce-simplify-code` returned four findings (reuse an existing subject matcher, collapse two
now-subsumed clauses, precompute trimmed prefixes, drop a near-duplicate test) and applied none;
both thermonuclear passes in review reached two of the same four. The architect confirmed the
refusal and declined to forward them to the main line as well, on the ground that both lines
carrying the same code matters more than any of them.
