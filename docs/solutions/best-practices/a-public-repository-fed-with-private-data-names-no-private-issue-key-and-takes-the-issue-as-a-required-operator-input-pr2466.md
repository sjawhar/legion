---
title: "A public repository fed with private data names no private issue key, and takes the issue as a required operator input"
category: best-practices
tags:
  - public-repository
  - private-data
  - manifest
  - changelog
  - test-fixtures
date: 2026-10-30
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2466"
---

# A public repository fed with private data names no private issue key, and takes the issue as a required operator input

- **Keep the private tracker's issue keys out of every added line**, not only out of the payload.
  Keys arrive through places that do not look like data: a changelog fragment's file name and its
  `Issue:` field, a committed manifest's issue field and its links to the files, test fixtures that
  copied real keys, and README pointers to a spec.
- **Where the code needs the issue, make it a required operator input** (here
  `LOADTEST_REPLAY_ISSUE`). Refuse to start without it, naming the variable, before any download or
  build, and document it in the README as `<issue>`. Keep the manifest to what the repository may
  hold: slugs, versions, sizes and sha256 values.
- **Use placeholder keys in fixtures** (`ACME-1`, `ACME-2`, …), keeping distinct keys distinct and
  their numeric order, and show that no pinned value changed.
- **Name a changelog fragment by the pull request** (`changie new --custom Issue=pr<number>`), not by
  the issue.
- **Check with a three-dot diff against the base**, outside `.legion/`:
  `git diff origin/main... -- . ':(exclude).legion/**' | grep -E '^\+.*<KEY>-[0-9]+'`. The two-dot
  form also shows the base's newer commits as reversed lines and reports keys that are not yours.
  The repository's private-names check knows only the names it was given, so it passes these keys.
- **Record the mapping where its readers look.** Removing the key moves the lookup into the private
  tracker: post one comment on the issue that holds the files, naming the manifest's file, the
  data's window, the part names and the variable's value, and check that a search on the part names
  finds it.

## Evidence

`packages/envoy/loadtest/replay.sh:476` refuses `replay-get` without `LOADTEST_REPLAY_ISSUE`;
commit 4f6d4d8e of this pull request made the change (fixtures to `ACME-n`, the manifest's issue
field and its 50 links removed, the fragment renamed), with `replay-plan` output byte-identical
before and after.

The pull request had passed review, testing and CI with 137 added lines naming keys: the changelog
fragment's file name, the manifest's issue field and links, about 90 real keys in test fixtures,
and README pointers. `.github/scripts/check-private-names.sh` passed them, because it checks the
deployment's repository, company and hosts, not the tracker's project keys. The architect's
acceptance sent it back for this alone. Afterwards a search on the part names did not reach the
issue holding the files until a comment named them.
