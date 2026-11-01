---
title: "A renumbered data migration is checked against main's new one, and the schema version proves nothing"
category: testing
tags:
  - migrations
  - renumber
  - schema_version
  - forward-merge
  - postgres
date: 2026-10-26
status: active
module: packages/daemon
related_issues:
  - "INFRA-225"
  - "sjawhar/legion#2188"
---

# A renumbered data migration is checked against main's new one, and the schema version proves nothing

Extends docs/solutions/testing/a-renumbered-daemon-migration-leaves-the-pods-shared-embedded-postgres-skipping-mains-file-at-the-old-number-and-refusing-its-own-recreate-the-public-schema-LEGION-694.md.

- A renumber is more than a rename when your migration changes data. Your backfill now runs after
  `main`'s new migration. Read that migration for every table your backfill reads or writes, and
  record the result in the handoff. "The SQL is byte-identical" says the text did not change. It
  says nothing about whether the rows the SQL reads changed.
- Never cite the store's schema version (`SchemaVersion`, which is `max(version)` from
  `schema_version`) as proof that a renumbered schema applied. On a stale database it reports the
  top number even though `main`'s file was skipped silently. The proof is a database created
  fresh for the run, and the proof must say so.
- When k of your files shift by m numbers, a stale database fails at the first file numbered above
  everything it recorded, not at the file that collided. Every skipped file in between gives no
  error, and that includes your own shifted files as well as `main`'s. So do not debug the
  migration the error names: reset the schema as the extended note says.

## Evidence

After `main`'s #2173 took 0051 (`issue_suspend_release`, which only rewrites `outbox` payloads),
this change's `dispatch_issues` and `issue_taken_back` moved from 0051/0052 to 0052/0053.

- The pod's long-lived test schema had recorded 51 and 52 from the old files. It skipped `main`'s
  0051 and the new 0052 with no error. It failed at 0053, a file whose SQL had not changed:
  `column "taken_back" of relation "issues" already exists`. 77 `internal/daemon` tests failed
  that way until the schema was reset.
- Had 0053 said `add column if not exists`, the daemon would have booted and reported version 53,
  without `main`'s outbox rewrite.
- Only the reviewer checked that 0053's backfill (which reads `issues`, `phases` and `claims`) does
  not read the `outbox` rows that `main`'s 0051 rewrites.
