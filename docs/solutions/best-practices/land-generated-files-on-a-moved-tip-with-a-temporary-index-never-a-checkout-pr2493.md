---
title: "Land generated files on a moved tip with a temporary index, never a checkout"
category: best-practices
tags:
  - git
  - plumbing
  - release-workflow
  - generated-files
  - shell
date: 2026-10-29
status: active
module: .github/scripts
---

# Land generated files on a moved tip with a temporary index, never a checkout

## The rule

1. A script that commits generated files onto a branch tip it is not standing on builds the commit
   with plumbing, in an index of its own:
   `GIT_INDEX_FILE=<tmp>/index git read-tree <tip>`, then
   `GIT_INDEX_FILE=<tmp>/index git add -A -- <only the output paths>`, `git write-tree`,
   `git commit-tree -p <tip> <tree>`, and `git update-ref --no-deref HEAD <commit>`.
2. Never `git checkout` the tip from a script that lives in the repository. When the tip carries
   another copy of the script, the checkout rewrites the file that bash is still reading.
3. Compare the written tree with `<tip>^{tree}` before you commit. Equal trees mean the tip already
   holds the output, so commit nothing.
4. Because the index starts as the tip's tree and `git add` names only the output paths, the commit
   cannot change any other file. Assert that in the test (one commit, one parent, the tip, and every
   changed path under the output directories). Do not leave it as a reviewer's belief.
5. Put the temporary index under a directory the script's cleanup removes. Remove it once
   `write-tree` has run.

## Evidence

- sjawhar/legion#2493: `release-push.sh push-built` lands a build made from base B on main's
  current tip T, after main moved while the push job waited for a runner. `commit_build_on`
  (`.github/scripts/release-push.sh:630`) is the rule above. The planner's gap analysis named the
  hazard in rule 2 ("Checking out the tip rewrites the running script", plan D3.7).
- `built_landed` (`.github/scripts/release-push.test.sh`, push-built section) asserts rule 4 on a
  local bare origin for every landing case.
- The real-history run on a clone of sjawhar/legion landed one github-actions[bot] commit whose one
  parent was main's moved tip, with 0 paths outside `dist/` and `legion/`
  (sjawhar/legion#2493, comment 6491547551).
