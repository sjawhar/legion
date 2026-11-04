---
title: "A measurement's subject is the shape the production caller produces"
category: testing
tags:
  - benchmarks
  - performance
  - postgres
  - fixtures
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/benchtime
related_issues:
  - "sjawhar/legion#2589"
---

# A measurement's subject is the shape the production caller produces

Bind a committed measurement's subject to what the production caller hands the statement:
the same exclusions, the same cardinality class, and the same number of inputs.

- Name in the test which call that subject is taken from, and the function that builds that call's inputs,
so a later reader can check the subject against the caller rather than against the seed.

- Read the subject from the database at the start of the run, under a floor that keeps it in the class the measurement is about,
and fail the run when the database holds no row at that floor rather than measuring a subject of the harness's own choosing.

- Reject a subject on which both texts answer from the first row they meet.
There both texts read the same small number whatever their plans are, and a committed "lower after" over it gates nothing.

- Apply the exclusions the caller applies.
An absence check with nothing excluded stops at the first matching row, which is not the statement production runs.

## Evidence

In sjawhar/legion#2589 the related-terms probe (`relatedProbeSQL`) is measured on twenty rare words of the project with the owner issue excluded,
and the test says why each half of that subject is what it is
(`packages/envoy/internal/dispatch/api/statement_timing_test.go:139-153`).
Twenty is the list a question of a paragraph or two hands the picker;
rare is the class the picker probes, because it probes only the words the project's statistics do not hold, the noise and the too-common already dropped (`pickRelatedTerms`);
and the owner is excluded because every write that runs the check excludes its own owner (`relatedExclusion`).

The floors that keep the subject in that class live in the harness:
`rareWordFloor = 100`, `mediumWordFloor = 1000` and `probeLexemeCount = 20`
(`packages/envoy/internal/dispatch/benchtime/subjects.go:58-67`),
with the field doc saying that on the project's commonest words "every arm matches its first row at once, the probe reads tens of milliseconds whatever its plan is, and a document arm that reads the whole table once per lexeme is invisible"
(`packages/envoy/internal/dispatch/benchtime/subjects.go:47-55`).
A word the database cannot supply at a floor fails the run instead of being replaced by one it can
(`packages/envoy/internal/dispatch/benchtime/subjects.go:153-161`).

The regression this subject exposed, and the earlier subject hid:

- On three lexemes a comment on the pull request measured the probe at 24.4 ms for the released text and 27.7–34.4 ms for this release's,
a difference inside the spread of either.

- On twenty rare words with the owner excluded, the pull request body records the same pair at "3.85 s against 7.51 s, this release slower than the one before it,
because the document arm was planned once per lexeme over the whole of `document_search` (`Seq Scan on document_search v, actual 383.804 ms, loops=20`)".

- With the subject fixed and the arm repaired, the body's protocol 1 table reads `relatedProbeSQL`, twenty rare words, owner issue excluded, at 7.444428 s before and 17.406 ms after.
