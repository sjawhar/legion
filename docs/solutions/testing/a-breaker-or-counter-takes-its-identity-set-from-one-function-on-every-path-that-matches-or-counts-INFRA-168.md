---
title: "A breaker or counter takes its identity set from one function on every path that matches or counts"
category: testing
tags:
  - circuit-breaker
  - identity
  - mutation-testing
  - pi-envoy
date: 2026-10-30
status: active
module: packages/pi-envoy
related_issues:
  - "INFRA-168"
  - "sjawhar/legion#2161"
---

# A breaker or counter takes its identity set from one function on every path that matches or counts

Extends docs/solutions/testing/a-de-duplication-is-proven-by-mutating-the-shared-function-and-rebuilding-every-surface-pr2511.md.

When a breaker, quota or counter acts on the things inside a call (the asks of a write, the items of an upload):

- **Derive the set of keys a call has in one exported function.** Have every path read that one function: the match that lets a resend pass, the count each refusal path adds, and the warning that promises what the next resend will do. A path that derives keys of its own is the defect. It returns in each review round until the derivation is shared, because each fix to one path leaves the others different.
- **Make the count a subset of the match.** A refusal counts only keys that the refused call's own key set holds. Then a warning that says "a third resend goes out" is true by construction.
- **Give each kind of call a key of its own.** A call that opens no item must not share a placeholder key (a whole-call key) with other calls on the same target. If it does, one call's refusals pass another call.
- **Lock it by mutating the shared function** (drop a part of a key, drop a content digest), and require tests that go red under each mutant. Tests that assert each path alone stay green when the paths agree with each other but are wrong.

## Evidence

Four review threads of one kind on sjawhar/legion#2161. Each was fixed by moving a path back onto the shared derivation:

- `PRRT_kwDORFy7ds6wNcAk` (the breaker counted a block the stored version held): 5283cb02.
- `PRRT_kwDORFy7ds6wOx3B` (the charter path counted stored blocks): f1f55963, one `refusalThings` rule for both refusal paths.
- `PRRT_kwDORFy7ds6wPhQS` (a warning with no pass behind it) and `PRRT_kwDORFy7ds6wPhQY` (an upload passed on another upload's whole-call counts): 632f8d02, one key set per call feeds the match and the count, and an upload's key is its own.

The landed shape is in `packages/pi-envoy/extensions/askgate/breaker.ts`: `callKeys` at `:65` and `refusalThings` at `:94`. In 2ac8abe5, three tests go red under both of the reviewer's `callKeys` mutants (the upload key dropped, the content digest dropped) and pass on the unmutated code.
