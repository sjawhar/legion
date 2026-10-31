---
title: "A changelog fragment claims only what the diff does, and every changed package with a .changie.yaml gets one"
category: legion
tags:
  - changelog
  - changie
  - claude-envoy
  - release-note
date: 2026-10-28
status: active
module: packages
---

# A changelog fragment claims only what the diff does, and every changed package gets one

Extends docs/solutions/legion/a-pull-request-never-commits-the-claude-envoy-build-and-ships-a-new-bundle-before-its-entry-pr2423.md.

1. Build the list of fragments from the diff, not from memory. List the packages your pull
   request changes (`jj diff --from main@origin --to @- --name-only`), and give each one that has a
   `.changie.yaml` a fragment under its `.changes/unreleased/`.
2. Check every sentence of a fragment against the diff at the head that merges.
   - A pull request does not regenerate claude-envoy's `dist/` or `legion/`, so its note says
     main's Rebuild Claude Envoy plugin workflow builds them from this commit's source after the
     merge.
   - When the contract changes, the note also says what runs until that rebuild lands: the
     committed bundle is the previous contract's build, which the new daemon refuses at
     registration.
3. A fragment that moved from a `CHANGELOG.md` edit is still a claim. Read it again at every
   merge that changes what the pull request ships.

## Evidence

- sjawhar/legion#2354:
  - Review round 5 found no fragment for `packages/envoy-client`, though its `dispatch-cli.ts`,
    `dispatch-execute.ts` and `dispatch-http.ts` changed. The fix is commit d0a5a0ca.
  - Review round 7 found the claude-envoy fragment still saying "`dist/` and `legion/` were
    regenerated" and "install it with a `legion` built from the same commit". Both were false
    after main's #2423. The fix is commit abdef8d9.
