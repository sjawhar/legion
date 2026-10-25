---
title: "A test-only knob widens the value production runs: pin it at the construction a deployment reaches"
category: testing
tags:
  - flaky-tests
  - timeouts
  - test-seams
  - regression-pins
  - dispatch-server
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "INFRA-56"
  - "sjawhar/legion#2158"
---

# A test-only knob widens the value production runs: pin it at the construction a deployment reaches

A test that fails because a real deadline and a loaded machine are too close is fixed by making
the deadline the test's own input, never by widening the assertion. The seam that does it is also
a new way for production's own value to drift, so the drift detector ships in the same change.

- **Make the bound an input, not a wager on the machine.** Put the override on the production
  struct as an unexported field, read it through one accessor whose zero value is the documented
  constant, and give the test two cases: a bound nothing can meet, and a generous one. Every
  caller a deployment reaches leaves the field zero, so nothing a user meets changes.
- **Never widen a wall-clock bound instead.** A widened ceiling is the next flake, and the margin
  is arithmetic you can do before you commit: run the candidate assertion under this repository's
  load recipe
  (`docs/solutions/testing/widen-the-contender-count-before-calling-a-race-unreproducible.md`)
  and read its worst case against the bound. A worst case within a tenth of the ceiling is a test
  that will go red on a busier runner; do not commit it.
- **Prefer an in-process read to a behavioural timing assertion for the pin itself.** The pin
  exists to catch a number drifting, and a stopwatch pin re-imports the flakiness the change was
  removing.
- **The pin calls the composition root, never a struct literal that looks like it.** `(&server{})`,
  `directServer(deps)` and every other hand-built value in a test file are blind to a default set
  at the real construction site. Call the function a deployment calls — `register`, the entry
  point, the factory — and assert what it hands out.
- **Prove the pin by drifting the production site.** Set the field at that site, watch the pin
  redden with the drifted value in its message, restore, and check the working copy is clean.
  A pin that has only ever been green is a claim.
- **Keep the seam unexported when a document promises the value to a user.** An exported field on
  a dependency struct lets a caller outside the package change a number the README and the
  changelog promise unconditionally; that is the one thing such a change must not do. The hazard
  it leaves — several construction sites that can disagree — is what the pin patrols.

Related: `docs/solutions/daemon/a-dependency-made-optional-for-one-runtime-is-refused-at-construction-for-the-other-and-proven-through-the-entry-point.md`
makes the same point for a dropped dependency in the TypeScript daemon;
`docs/solutions/testing/a-golden-fixture-is-not-the-deployment-cite-the-live-assertion-for-a-live-fact-INFRA-69.md`
is the fixture-shaped version of "assert the live thing".

## Evidence (INFRA-56, sjawhar/legion#2158)

`TestCreateAskOnIssueKeepsSuggestionsWhenWriteAdviceFails` filed an ask through
`POST /api/v1/issues/{key}/asks` and asserted that four Postgres steps sharing one 300 ms
allowance (`writeSuggestionTimeout`, `internal/dispatch/api/suggestions.go:21`) had all finished
inside it. Under three copies of the same suite the four took 54 ms at the median and 277 ms at
their worst over 600 runs, and the test failed once in 40 on the answered-question lookup. The
fix is `server.suggestionTimeout` read through `suggestionAllowance()`, with the test's two cases
at `time.Nanosecond` and `30 * time.Second`; the seam was proven causal by reverting the one
`context.WithTimeout` to the constant, which turned the forced case green while the generous one
stayed green.

The knob then widened production's surface, and the first pin did not see it. Written as
`(&server{}).suggestionAllowance()`, it stayed green while `register` — the only construction a
deployment reaches — was drifted to `&server{deps: deps, suggestionTimeout: 5 * time.Second}`:
filing an ask took **5.046 s** against the documented 300 ms, and all 41 tests of the suggestion
set, both sibling deadline tests included, stayed green. Rewritten as
`register(http.NewServeMux(), Deps{}).suggestionAllowance()`, it fails with
`the server a deployment runs allows 5s for its suggestions, want 300ms`, and with `allows 10ms`
when the constant itself is changed. The tester measured the drift, the reviewer re-measured the
pin independently through a Go `-overlay` that left the workspace untouched.

The behavioural version of that pin was written, measured and deliberately not committed: an ask
filed with `write_suggestions` held under `lock table … in access exclusive mode`, asserting the
reply within `writeSuggestionTimeout + 500 ms`. Under the load recipe its 40 filings ran 316 ms
minimum, 372 ms median and **737 ms worst against an 800 ms bound**. A 63 ms margin is the next
flaky test, so the measurement became the round's real-surface proof instead of a committed
assertion.

Still open in that package: `TestComputeAndPersistSuggestionsRespectsDeadlineUnderLock` and
`TestComputeAndPersistSuggestionsRespectsDeadlineWhenProjectLookupIsLocked`
(`suggestions_test.go:290`, `:326`) assert elapsed time against that same
`writeSuggestionTimeout + 500 ms` ceiling, doing locked-table work on the same Postgres. The
arithmetic above indicts them; INFRA-56 left them unedited by its own scope and reported them.
