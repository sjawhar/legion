---
title: "A database-major census greps the version tags too, not only the old digest and the mount path"
category: infra
tags:
  - postgres
  - major-upgrade
  - census
  - grep
date: 2026-10-29
status: active
module: .github/workflows
---

# A database-major census greps the version tags too, not only the old digest and the mount path

Extends docs/solutions/infra/the-postgres-18-image-mounts-its-data-one-level-up-so-move-the-mount-with-the-image-pr2364.md.

- When you claim that no site runs the old major any more, grep for three things: the old digest,
  the old mount path (`/var/lib/postgresql/data`), and the version tags (`postgres:1[0-7]`,
  `pg1[0-7]`). A `postgres:16` tag with no data mount matches neither of the first two greps.
- Grep the whole tree, not only `.github/workflows`. Developer-facing text pins a major too: a test
  helper's skip message that prints a `docker run` line, devbox e2e scripts, READMEs. For each hit,
  either move it with the change or name it on the pull request as left on the old major, with the
  reason.

## Evidence

- At a8808d54, the tester ran `grep -rnE 'postgres:1[0-7]|pg1[0-7]' .github/workflows` beside
  `grep -rn 7b822b0a .github/workflows`. Both were clean, so no CI job runs PostgreSQL 16.
- The same tag grep over the tree finds `postgres:16` in sites the digest grep cannot see. Among
  them are `packages/daemon/internal/testpg/testpg.go:24`, the skip message a developer copies,
  and `scripts/e2e/stage1-skeleton.sh:152` (`test.json` `observations`). Following that skip
  message tests the daemon against 16, while CI and production run 18.
