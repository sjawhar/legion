---
title: "A test that extends a shipped set clips it at a bound the code owns, and stages the set's next member to prove it"
category: testing
tags:
  - fixtures
  - migrations
  - go
date: 2026-10-31
status: active
module: packages/daemon/internal/store
related_issues:
  - "sjawhar/legion#2411"
---

# A test that extends a shipped set clips it at a bound the code owns, and stages the set's next member to prove it

- A test that adds crafted members to a set the product keeps growing (embedded migrations, registered handlers, a shipped list) must not take the whole set.
  Name the property its assertions read (the highest member, the first of a kind, a count), and keep only the part that growth cannot reach.
- Read that bound from the code that defines it, never write the value in the test.
  When no such bound exists, craft the whole set instead, as `craftedSet` in `packages/daemon/internal/store/migrations/embed_test.go` does.
- "The next real member will not break this test" is a claim to prove, not to reason about.
  Add a throwaway member to the real set, watch the test fail before the fix and pass after it, then delete the member and confirm none is left.

## Evidence

Three tests in `packages/daemon/internal/store/migrate_timestamped_test.go` appended crafted timestamped migrations to `migrations.All()`, and one started from `store.Migrate` over the whole set.
The reviewer approved them, and READY was armed.
Another tree preparing the daemon's first real timestamped migration (`20261031150000_handoff_records`, sjawhar/legion#2354) found they would fail: its migration would be the highest version, and it would widen the version column first.
The fix (`d910aa0c`) keeps the migrations up to `migrationid.ClosedAt` of the embedded set, and the widen test starts from that numbered set.
With a throwaway `20261101000000` migration embedded, the three tests failed at `475a0929` and passed at `d910aa0c`.
