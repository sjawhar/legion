---
title: "A forward merge applies every rule main added to an AGENTS.md to the files the branch added, not only to the conflicted ones"
category: legion
tags:
  - forward-merge
  - agents-md
  - playwright
  - changelog
date: 2026-10-28
status: active
module: packages/dispatch/e2e
---

# A forward merge applies every rule main added to an AGENTS.md to the files the branch added, not only to the conflicted ones

Extends docs/solutions/legion/a-forward-merge-also-ports-mains-prose-about-a-component-the-branch-moved-LEGION-247.md.

That document ports main's prose about a component the branch moved. Here main adds a *rule*, and the branch's own files, written before the rule existed, fall under it with no conflict marker.

- **After a merge of main, list every `AGENTS.md` main changed in the merged range:** `jj diff --from <last base> --to main@origin --name-only | grep AGENTS.md`. Read each added rule against every file your branch added or rewrote.
- **An opt-in rule fails nothing when your file is left out.** A Playwright project that runs only tagged tests, or a fragments directory the changelog fold reads, silently skips a file that predates the rule. Apply the rule in the merge itself, so the merged head is the one the tester checks.
- **Prove the result with the rule's own selector.** For a test tag, run `playwright test --project=<project> --list` over the branch's files and check that every test the rule names is listed.
- **Record it as part of the conflict resolution,** with the rule's location, in the merge record on the pull request. It moves the diff fingerprint, so the record says why.

## Evidence

On sjawhar/legion#2324, the merge of main `4e55b2e0` (commit `08fcdf72`, PR comment 6466073672) brought in two such rules:
- **#2313:** the `iphone` project runs only tests tagged `@phone` (`packages/dispatch/AGENTS.md`, "Phone acceptance", rule R1: a test that branches on the `iphone` project takes the tag). Only two of the branch's test titles conflicted. Seven more branched on `iphone` (the refetch-budget sidebar, pin and replies tests; refresh-freshness reference and inbox-closed; live's project-list first load; inbox's later-response test). Untagged, they would have stopped running on iphone with nothing turning red. `--project=iphone --list` showed all of them selected after the tags.
- **#2419:** changelog entries are fragment files (root `AGENTS.md`), so the branch's entries under the old heading moved into `.changes/unreleased/` fragments.
