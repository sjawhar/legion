---
title: "A test of a margin stands off its boundary and reads the margin from the code"
category: testing
tags:
  - go
  - clocks
  - boundaries
  - export_test
  - fakes
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/redeliver
related_issues:
  - "sjawhar/legion#2641"
---

# A test of a margin stands off its boundary and reads the margin from the code

For a rule of the form "act only once this is more than M old". The control direction is
docs/solutions/testing/a-regression-row-fails-for-the-mechanism-it-names-INFRA-66.md; these are the
three ways such a test passes while proving nothing.

## Rules

- **Put each case strictly inside and strictly outside the margin, never on it.** A case exactly at
  M passes whether the code compares against M or against M minus a unit, so a reviewer changing the
  margin by one unit sees green. Assert nothing happened just inside, and the act just past.
- **A margin measured against a value that was truncated needs a clock that is off the
  truncation's grid.** When the key under test is truncated to its minute and the margin is two
  minutes, a clock sitting on a minute boundary makes "margin" and "margin minus the truncation"
  the same instant, and the case cannot discriminate. Move the fake clock to a known off-grid point
  (here the next `:30`) and say in the test why, together with what else that move affects — here
  the sweeper's clock only, since the database's clock judges a recorded rate limit and the test
  records none.
- **Read the margin and the key from the code, through `export_test.go`.** Export the production
  constant and the key builder (`const CursorOverlap = cursorOverlap`,
  `func PendingKey(...) string { return pendingKey(...) }`) and have every test outside the package
  build its keys through them. A test that spells `2 * time.Minute` or formats the key itself pins a
  copy, and the copy and the code drift apart silently.
- **Assert a stored value's shape only where a second reader parses it; inside one package assert
  the behaviour.** A test that pins a marker's JSON and its timestamp stops holding what the marker
  is *for* — that the next pass does or does not read the whole source. Drive that through a fake
  that counts the reads instead. Pin the stored form where two builds read it, which is the
  cross-build case, and nowhere else.

## Evidence

- `packages/envoy/internal/dispatch/redeliver/export_test.go`: `PendingPrefix`, `PendingKey`,
  `CursorOverlap`, each with the reason a test reads it from there.
- `TestAStrayIndexEntryIsDeletedOnlyPastTheMargin` (`sweep_test.go:1594`): the clock moved to the
  next `:30` with its reason inline, and the three strays (fresh, edge at
  `Truncate(time.Minute).Add(-CursorOverlap)`, and five minutes old) — only the last is deleted.
- `countingBucket` (`sweep_test.go:1645-1668`) counts the whole-bucket scan and the window read, so
  the marker's effect is asserted where a test had been asserting its JSON.
- The last review round that found items (round 4) raised all three: an assertion pinning the
  marker's stored form, a margin test hard-coding `2*time.Minute`, and a marker test whose seeded
  empty value no longer meant what the test assumed.
