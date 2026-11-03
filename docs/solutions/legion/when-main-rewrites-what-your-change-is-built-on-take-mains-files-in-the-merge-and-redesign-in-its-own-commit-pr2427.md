---
title: "When main rewrites what your change is built on, take main's files in the merge and redesign in its own commit"
category: legion
tags:
  - forward-merge
  - conflict-resolution
  - fingerprint
  - jj
date: 2026-10-28
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2427"
---

# When main rewrites what your change is built on, take main's files in the merge and redesign in its own commit

Extends docs/solutions/legion/conflict-only-rebases-keep-the-diff-auditable.md.

- When a conflict shows that main replaced the code one part of your change relies on, do not
  write the new design into the merge commit. In the merge, restore main's version of the files
  that part touched (`jj restore --from 'main@origin' <paths>`) and delete the part's own new
  files. Keep the parts that still apply. The merge commit then adds no line of new design.
- Build, then run the tests of the parts you kept, on the merge commit before you move off it.
- Write the redesign on main's code as the next commit, with its own tests.
- Post the fingerprint as changed. Take every proof again against the new base, and say in the
  pull request body that the earlier measurements no longer count.
- Before restoring a whole file from main, check that no other part of your branch edited it,
  or put that part's hunks back by hand.

## Evidence

- Main's #2360 replaced two inline readers with three. Merge 0413af1e restored main's
  `emphasis.go`, `parse.go`, `render_inline.go` and `nesting_test.go` and dropped
  `flanking_test.go`. It kept the other two parts (`marks.go`, `render.go`). Commit 7edefc89 then
  redesigned the read-back skip on the three readers.
- Fingerprint d1f759aa… → 4b7e79f9…, changed
  (https://github.com/sjawhar/legion/pull/2427#issuecomment-6468240380). Every proof was taken
  again at the new base.
- A later conflict with #2361 touched only an import block. Its fingerprint was unchanged
  (https://github.com/sjawhar/legion/pull/2427#issuecomment-6473348407).
