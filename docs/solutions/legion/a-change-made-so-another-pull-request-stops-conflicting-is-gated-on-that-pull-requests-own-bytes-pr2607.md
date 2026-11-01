---
title: "A change made so another pull request stops conflicting is gated on that pull request's own bytes, and the merge probe is read as a delta"
category: legion
tags:
  - conflicts
  - forward-merge
  - git-merge-tree
date: 2026-10-31
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2607"
---
# A change made so another pull request stops conflicting is gated on that pull request's own bytes, and the merge probe is read as a delta

Extends docs/solutions/legion/a-branch-that-carries-an-open-sibling-pull-requests-files-takes-the-siblings-bytes-and-proves-its-own-delta-when-the-sibling-merges-pr2474.md, which takes the sibling's bytes into a branch that carries its files.
This is the other direction: a change landing on the base so that the sibling stops conflicting.

- Produce the shared lines by copying the other pull request's bytes, not by writing your own version of the same deletion: `jj restore --from <its head> <paths>`.
  Gate on byte identity, `jj file show -r <its head> <f> | diff - <f>`, and state in the pull request body the exact output you expect for each file, including the lines you keep on purpose.
- A grep that proves the old names absent is not that gate.
  A deletion in one branch surfaces in the other as a *rewrite* wherever it renames, renumbers or re-points a neighbour, and a rewrite is invisible to an absence grep: your own wording of those lines passes every name check and still conflicts.
- Restore only the files where you want every hunk of theirs.
  A file that carries a second hunk which must outlive the other pull request is edited by hand, with their exact replacement text, and its expected diff is that one hunk.
- `git merge-tree --write-tree --name-only <your head> <their head>` answers "does my head *add* a conflict with theirs", never "will theirs now land cleanly".
  Run it from your base as well, and make the evidence the equality of the two conflicted-path lists.
  A probe whose answer is the same at base and at head is no evidence for a head-specific claim.
- The claim that you removed a *future* conflict needs other evidence: enumerate, at both heads, every write the producer makes to the contested path, and put the rest on a check after the merge.

## Evidence

- sjawhar/legion#2607 exists so that sjawhar/legion#2599, which deletes `packages/envoy-plugin`, stops conflicting with `main`.
  Several of #2599's `release.yaml` hunks are rewrites rather than deletions: `envoy_plugin_build:` to `pi_envoy_build:` with its comment block rewritten, `as envoy_plugin_build says` to `as pi_envoy_build says` at two sites, and `four build jobs` to `three build jobs` in `release.yaml` and in `.github/scripts/check-release-credentials.sh:8`.
  Any other wording of those lines satisfies "no `envoy_plugin` name remains" and leaves the conflict standing.
- The gate, run at the pushed head against #2599's head: `changelog-fold.yaml` and `check-release-credentials.sh` byte-identical; `release.yaml` exactly `14a15`, the one trigger line kept because the worker image still reads it; `AGENTS.md` exactly `88c88`, the typecheck list that must outlive #2599.
  `AGENTS.md` was therefore not restored and its one line was edited by hand.
- The probe at the head conflicted in exactly `packages/envoy-plugin/package.json`.
  So did the same probe from `main`: git takes the other side wherever your base changed nothing since the merge base, so the single-path answer was the state before the change too.
  The failure the probe does catch is that list *growing* by one of the files you touched, which is what a reworded deletion would have produced.
- The replacement evidence for the future conflict: four `release-push.sh push-main|tag` write calls at the head against six on `main`, and one `packages/envoy-plugin` line in the file against twelve.
  The rest went to the production check after the merge: tag-set equality and `git log <merge>..origin/main -- packages/envoy-plugin/` empty.
- The tester improved the gate where byte identity is unreachable: diff the contested files *out of the merged tree* against the other side's versions.
  At merged tree `f6cd7013` all four came out byte-identical to #2599's own, `release.yaml` included, because #2599 deletes the one line #2607 keeps.
