---
title: "Ask a scratch repository what git add will skip, and never read a status through a pipeline"
category: best-practices
tags:
  - git
  - gitignore
  - shell
  - pipefail
date: 2026-10-28
status: active
module: .github/scripts
related_issues:
  - "sjawhar/legion#2423"
---

# Ask a scratch repository what git add will skip, and never read a status through a pipeline

## The rule

1. To learn which generated files a later `git add -A` will leave out, lay the files out at their
   repository path in a scratch git repository, copy in the repository's `.gitignore` files on
   that path, and run `git ls-files -o -i --exclude-standard -- <paths>` there. A
   `git -C <checkout> check-ignore` over a list of paths never applies a `.gitignore` that the
   generated tree itself carries.
2. Capture each listing in its own assignment and give a failure its own error:
   `x=$(cmd) || { echo "::error::<script>: <what failed>" >&2; exit 1; }`. Never branch on the
   status of `$(producer | consumer)`: under `pipefail` the status is the rightmost failing
   command's, so a producer that failed can read as the consumer's "no match".

## Evidence

- sjawhar/legion#2423, review round 4, `packages/claude-envoy/scripts/verify-build.sh`: the
  ignored-file check ran `find … | git -C "$repo_root" check-ignore --stdin` and branched on its
  status. A skill shipping `.gitignore` naming `notes.md`, with that `notes.md`, passed, though
  push-built's `git add -A` would have left `notes.md` off main; and
  `bash -c 'set -euo pipefail; s=0; x=$( (echo packages/a.log; exit 1) | cat ) || s=$?; echo "status=$s ignored=$x"'`
  printed `status=1 ignored=packages/a.log`. Fixed in 4360b7a2 (scratch repository,
  `git ls-files`), with its own `::error::` line in c3a63d7e.
