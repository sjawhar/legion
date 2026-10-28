---
title: "A per-request decode memo keys on the stored bytes, clips what it shares, and leans on insert-only columns"
category: envoy
tags:
  - dispatch
  - jsonb
  - decoding
  - memo
  - etag
  - performance
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2425"
---

# A per-request decode memo keys on the stored bytes, clips what it shares, and leans on insert-only columns

When one read decodes the same jsonb value on many rows, decode it once per request:

- **A value that is the same on every row of one parent** (a column of the joined parent row):
  decode it on the parent's first row, found by its presence in the result map. Do not compare it
  with the previous row: that depends on the query's order.
- **A value that repeats across rows** (a list such as `artifact_versions.authors`): keep a local
  `map[string]T` from the stored bytes to the decoded value.
  - Look up with `m[string(raw)]`. Go does not allocate for that lookup, and the store copies the
    key. pgx v5 already gives a `*[]byte` target a fresh copy for each row
    (`scanPlanJSONToByteSlice` in `pgtype/json.go`), so the copy matters only when the bytes alias
    the driver's buffer, as a `pgtype.DriverBytes` target does.
  - Use the two-value lookup and decode a miss into a fresh variable, so JSON `null` (a nil slice)
    and `[]` (an empty slice) stay two entries.
  - Store `slices.Clip(decoded)`. Then a later `append` on one row's copy allocates and does not
    write into the array the other rows share.
  - Never store a failed decode. Return the same error text as before, so the first bad row still
    fails first.
- Keep the memo a local of the one call: no package cache and no `sync.Pool`. Write the read-only
  invariant as a comment at the memo, naming the caller that only encodes the result. A future
  caller that changes one element changes every row that shares it.
- When the response's ETag does not hash the memoized fields, show that their columns are
  insert-only. List every `insert` that writes them, and grep the Go code and the migrations for
  any `update` of those columns. Say in the pull request that a new value always comes with a new
  row, which the ETag does hash.

## Evidence

- `loadArtifactsByRefKey` (`packages/envoy/internal/dispatch/api/references.go`) decoded
  `a.created_by` and `v.authors` on every version row. In production, distinct author lists were
  6.2% of version rows. The memo cut the decode's profile share from 2.62% to 0.16% on the load
  test, and its allocations from 351–360 MB to 20–29 MB per run.
- The regression test `TestIssueReferencesMembersCarryTheirDocumentsOwnCreatorAndVersionAuthors`
  (`references_test.go`) uses documents with 1, 40 and 3,217 versions and 12 author lists,
  including `null` and `[]`. It compares each member with the unmemoized `loadVersions` read and a
  direct `created_by` select. It fails for a memo keyed on the document, for `null` turned into
  `[]`, and for one creator reused for all documents.
- `referencesETag` hashes only the event sequence, the truncation flag, and each member's id and
  last version number. `artifacts.created_by` is written only by inserts (`api/artifacts.go`,
  `api/issue_create.go`). `artifact_versions.authors` is written only by inserts (`api/artifacts.go`,
  `api/issue_create.go`, `docs/mutation.go`). No update of either column exists in the production code or
  in the migrations; only tests update them, to build fixtures.
