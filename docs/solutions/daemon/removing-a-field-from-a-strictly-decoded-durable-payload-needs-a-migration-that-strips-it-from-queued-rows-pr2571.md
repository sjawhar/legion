---
title: "Removing a field from a strictly decoded durable payload needs a migration that strips it from the rows already queued"
category: daemon
tags:
  - migration
  - outbox
  - strict-decoding
  - rolling-upgrade
date: 2026-10-31
status: active
module: packages/daemon/internal/store
related_issues:
  - "sjawhar/legion#2571"
---

# Removing a field from a strictly decoded durable payload needs a migration that strips it from the rows already queued

Extends docs/solutions/daemon/a-guard-column-a-new-sweep-reads-is-backfilled-from-traces-nothing-prunes-and-proved-from-the-previous-migration-INFRA-252.md,
which covers a column a new release adds. This is the same discipline for a field a release
**removes** from a payload the previous release has already written into a durable queue.

Deleting a field from a payload struct looks finished when the code compiles and the suite passes.
The rows the previous release queued are the part no compiler sees. Under a strict decoder
(`DisallowUnknownFields` or equivalent), each of those rows fails to decode on every attempt, so
the row is retried for ever and its lane stalls — worse than a wrong value, because nothing
finishes and nothing reports.

So a removal ships with a migration that strips the key from the rows in flight:

```sql
update outbox set payload = payload - 'quiesce' where kind = 'supervise' and payload ? 'quiesce';
```

and with a test that seeds at the previous migration, reads the decode refusal **before** the
migration as its negative control, migrates, and asserts the row decodes to exactly the payload it
should, every other field untouched.

Add a new value to the enum the same release needs in its own migration, and give that one the
same before-and-after shape: the constraint refuses the new kind before, takes it after.

## Evidence

- `packages/daemon/internal/store/migrations/0065_supervise_quiesce.up.sql` strips the `quiesce`
  key a supervise start carried before the CI-red takeover was deleted; its header states the
  stall the strip prevents.
- `migration_0065_test.go` seeds a start carrying `quiesce` at migration 64, reads
  `unknown field "quiesce"` as the refusal before the strip, migrates, and asserts the start then
  decodes and runs as the start it already was.
- `migrations/0066_phase_ended_kind.up.sql` adds `phase_ended` to the outbox's kind constraint, and
  `migration_0066_test.go` reads the constraint refusing the kind before and taking it after —
  the same shape for the addition the same release needed.
