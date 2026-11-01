---
title: "A Dispatch migration takes the next free number at each merge of main, never a reserved block, and its renumber moves every reference"
category: database-issues
tags:
  - migration
  - migration-number
  - pgmigrate
  - forward-merge
  - concurrent-pull-requests
date: 2026-11-01
status: active
module: packages/envoy/internal/dispatch/store
related_issues:
  - "sjawhar/legion#2395"
---

# A Dispatch migration takes the next free number at each merge of main, never a reserved block, and its renumber moves every reference

Extends docs/solutions/legion/schema-bump-branch-rechecks-mains-version-at-every-rebase.md.

- **Take the next free number on main, at every merge of main.** Dispatch's migration set must be
  numbered 1 to N with none missing (`TestMigrationSetIsNumberedOneToN`,
  `internal/dispatch/store/store_test.go`, through `pgmigratetest.CheckNumberedOneToN`). The runner
  itself would apply a gap (`internal/pgmigrate/set.go`, `Load`'s doc); the test refuses it so a
  lower number can never merge after a higher one and run after it in production.
- **Never reserve a block of numbers for concurrent pull requests.** A block above main's last
  number is a gap, so that test is red on the pull request until every lower block has landed,
  and whichever lands first may run before a lower number that lands later. When two open pull
  requests hold the same number, the one that lands second renumbers: its merge-queue run fails
  the numbering test or the shared-version check, and it comes back.
- **A renumber moves every reference, not only the file names.** Rename the forward migration and
  its `.census.sql` together, then search the repository for the old names and bare numbers: the
  server README's migration list, the package guides, the changelog fragment, architecture notes,
  comments in code and in other migrations, your own `docs/solutions` lessons, and the pull request
  body. Leave a mention of the old number that now names main's migration alone.
- **Prove the renumber by the per-file diff.** Compare each file's diff from its fork point before
  and after the merge: only the renamed migrations and the files that name them may differ, and
  the SQL only where a comment names a migration number.
- **Run the store suite where it can create roles and event triggers.** It applies every migration
  to a fresh database and needs superuser rights; on a Postgres without them it fails for
  privileges (`permission denied for schema public`, `pg_read_all_stats`, event triggers,
  `max_wal_senders`), not for the migrations.

## Evidence

- Main's #2541 took `0150_dpop_proofs` while this pull request held `0150_duty_holds` and
  `0151_outbox_claim`. The forward merge `d99681ce` renamed them to `0151_duty_holds` and
  `0152_outbox_claim`; diff check
  https://github.com/sjawhar/legion/pull/2395#issuecomment-6551902095. Of 120 files in this pull
  request's own diff, 14 differed, all of them the renumbering; the SQL changed in one comment.
- A block (`0158`/`0159` for this pull request, `0151`-`0157` and `0160` for two others) was
  proposed and withdrawn once `CheckNumberedOneToN`'s refusal of gaps was pointed out.
- `internal/dispatch/store` passed at `d99681ce` on a superuser Postgres 17 (255.3 s, the numbering
  test included). On a pod's test Postgres whose role lacked those rights, ten store tests failed
  for privileges alone.
- The nearest document, `docs/solutions/legion/schema-bump-branch-rechecks-mains-version-at-every-rebase.md`,
  covers the daemon's single state version, where the number cannot be shared; it says nothing of
  a gap-free numbered set, reserved blocks, or the references a renumber must move.
