---
title: "A branch that holds a migration number re-reads the base's highest at every push, and the number's cost is the count of files that cite it"
category: envoy
tags:
  - dispatch
  - migrations
  - pgmigrate
  - forward-merge
  - renumber
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch/store/migrations
related_issues:
  - "sjawhar/legion#2546"
---

# A branch that holds a migration number re-reads the base's highest at every push, and the number's cost is the count of files that cite it

Extends docs/solutions/envoy/an-approved-head-waiting-on-its-merge-rechecks-mains-migration-numbers-at-every-wake-and-only-a-push-can-send-it-back-pr2483.md.

Those notes say when to re-check the number. Two things follow for a branch that holds one for
days.

- **A far-off number is no escape.** Leaving a gap to avoid the next collision does not work:
  the repository's numbering check refuses a set that is not one-to-N, so the number must stay
  contiguous with the base's highest and moves every time the base's highest does. Expect one
  renumber per forward merge, not one in total.
- **When you pick the number, count the files that will cite it**, because that count is what
  each renumber costs and it is paid again at every merge. The citations are not only the
  migration files: the test's version constants, code comments that explain which release the
  behaviour arrived in, test comments, the package guide and the route reference all spell it.
- **Keep the number out of everything that can derive it.** A test constant is unavoidable; a
  prose sentence that says "since migration NNNN" is a copy that must be swept by hand. Prefer
  one sentence in one guide over the same sentence in four.

## Evidence

sjawhar/legion#2546 paid three forward merges for the migration number alone and nothing else:
`0145/0146` → `0148/0149` (`5012e1a9ac07`) → `0149/0150` (`c50588e7588d`) → `0150/0151`
(`1b191490e1ba`), each described `merge: take main's migration numbers`.

The middle one is the clean measurement, since it changed nothing else: eleven files, 24 lines.
The number lives in the two migration files and their two `.census.sql` headers, and is cited by
`packages/envoy/internal/dispatch/store/ask_blocks_migrations_test.go`'s two version constants
and their comment (`beforeAskBlocks`, `askBlockTables`, at `:13-19`),
`packages/envoy/internal/dispatch/api/ask_rows.go`,
`packages/envoy/internal/dispatch/docs/block_index.go:67`,
`packages/envoy/internal/dispatch/api/ask_parts_test.go:25`,
`packages/envoy/internal/dispatch/refs/ask_blocks_test.go:452`, four sentences of
`packages/envoy/AGENTS.md` (now at `:1059`, `:1060`, `:1269`, `:1706` and `:1730`, a fifth added
after the last renumber) and two of `packages/envoy/cmd/dispatch/README.md` (`:1245`, `:1261`).

The gap rule is `CheckNumberedOneToN`
(`packages/envoy/internal/pgmigrate/pgmigratetest/pgmigratetest.go:87`), whose own tests refuse a
gap and a set not starting at one (`pgmigratetest_test.go:119`, `:131`).
