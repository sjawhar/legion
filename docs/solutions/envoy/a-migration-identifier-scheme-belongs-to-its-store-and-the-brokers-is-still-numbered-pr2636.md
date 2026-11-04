---
title: "A migration identifier scheme belongs to its store, and the broker's is still numbered 1 to N"
category: envoy
tags:
  - migrations
  - pgmigrate
  - broker
  - forward-merge
date: 2026-11-04
status: active
module: packages/envoy/internal/broker/store/migrations
related_issues:
  - "sjawhar/legion#2636"
  - "INFRA-958"
---

# A migration identifier scheme belongs to its store, and the broker's is still numbered 1 to N

Extends docs/solutions/envoy/a-migration-added-after-the-numbered-series-is-named-for-its-creation-instant-and-a-guard-compares-the-applied-set-pr2411.md.

That note retired renumbering: a new migration is named for the UTC moment it was created, so two
open pull requests no longer take one number. It is right for the sets it names, and a reader who
applies it to the broker writes a file the broker's own guard refuses.

- **Read which scheme your store uses before you name the file, not after.** `migrationid.Check`
  holds Dispatch's set to timestamps and `pgmigratetest.CheckNumberedOneToN` holds the broker's,
  "which is numbered throughout" (`packages/envoy/internal/pgmigrate/set.go:63-65`); the broker's
  own guard is `packages/envoy/internal/broker/store/migrate_guard_test.go:20-23`, "The broker's
  set is numbered 1 to N with none missing".

- **For the broker, the retired remedy is still the live one.** A broker migration that collides
  with main's keeps main's number on main's file and takes the next free number — the behaviour
  docs/solutions/envoy/two-pull-requests-take-the-same-migration-number.md describes and its
  supersession notice says is no longer done.

- **Two places will tell you otherwise, and both are read before the guard runs.** The superseding
  note's prologue is unscoped — "nothing is renumbered any more", "Never renumber or rename a
  numbered file" — while the bullet under it scopes the timestamp to "a new daemon or Dispatch
  migration". And the shared loader's own refusal prescribes the timestamp to every caller,
  the broker included: "give each of the rest the UTC moment it was created, 14 digits
  YYYYMMDDHHMMSS" (`packages/envoy/internal/pgmigrate/set.go:143`). Following that line on a
  broker migration produces a filename `CheckNumberedOneToN` then refuses.

## Evidence

In sjawhar/legion#2636 a forward merge brought main's `0024_broker_outage_rule.up.sql` beside this
branch's `0024_drop_vm_enrollment_kind.up.sql`. Two files at one version with different names
conflict in no merge tool, and `pgmigrate.Load` refuses the whole set, so every test in
`packages/envoy/cmd/broker` failed on one line that never mentions the merge and no broker booted at
all. The collision and that symptom are already written down; what was not is which remedy the
broker takes. The file was renumbered to `0026` — 25 being the same branch's own `person_signins` —
rather than given the 14-digit name the loader's message asks for, because
`TestMigrationSetIsNumberedOneToN` holds the broker's set to 1 to N; that test and
`TestMigrationTwentySixRetiresVMEnrollmentsAndDropsTheKind` both pass at the renumbered name, and
`cmd/broker` passes whole.
