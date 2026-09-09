---
title: "An integration seam needs a live probe with the producer's exact value, not an assumed shape"
category: testing
tags:
  - integration-testing
  - legion
  - contract-testing
  - environment-variables
date: 2026-09-09
status: active
module: envoy-client
problem_type: integration_issue
component: api_layer
symptoms:
  - "Both sides' unit tests pass in isolation, yet the composed behavior is wrong"
  - "A dispatch tool call prefilled its issue argument as owner/repo#owner/repo#42 (double-prefixed)"
  - "The defect surfaced only when a live-stack probe used the daemon's real LEGION_ISSUE value"
root_cause: missing_validation
resolution_type: code_fix
related_components:
  - daemon
severity: high
---

# An Integration Seam Needs a Live Probe With the Producer's Exact Value

## Problem

The Legion daemon sets `LEGION_ISSUE` to the full reference it already tracks — an
`owner/repo#n` string for an external ref, or a native key for a root-issue tree
(`packages/daemon/src/daemon/processes.ts:942`, `packages/daemon/src/daemon/processes.ts:1147`),
and its tests pin the exact form: `packages/daemon/src/daemon/__tests__/processes.test.ts:413`
sets `"LEGION_ISSUE=sjawhar/legion#42"`. `envoy-client`'s dispatch tool executor consumes
`LEGION_ISSUE` to prefill an omitted `issue` argument
(`packages/envoy-client/src/dispatch-execute.ts:172-186`). Before this PR's thermonuclear deep
review caught it, the executor's prefill path assumed `LEGION_ISSUE` was always a bare issue
number and unconditionally built `${repo}#${legionIssue}` — so when it actually received the
daemon's real value, `sjawhar/legion#42`, it produced `owner/repo#sjawhar/legion#42`: a
double-prefixed, unresolvable reference.

Both sides' unit tests were green. envoy-client's own tests
(`packages/envoy-client/src/__tests__/dispatch-execute.test.ts:27-113`) set `LEGION_ISSUE` to
values the *test author* chose — a bare number, then (after the fix) a native key and a full
external ref — and asserted the executor's own logic against them. The daemon's tests assert
that it *sets* `LEGION_ISSUE` correctly, not what any particular consumer does with it. Neither
suite exercised the seam: what the daemon actually puts in the environment variable, fed to
what the executor actually does with it.

## What Didn't Work

- Trusting each package's own test suite as proof the integration works. A contract between
  two packages that each mock the other's half of the exchange records the *author's belief*
  about the contract twice, not the contract itself.

## Solution

The fix (verified in the current tree,
`packages/envoy-client/src/dispatch-execute.ts:172-186`) makes the executor check which shape
`LEGION_ISSUE` actually has before assuming it needs a repo prefix:

```typescript
const legionIssue = env.LEGION_ISSUE;
if (!legionIssue) throw new Error("issue is required; supply issue or set LEGION_ISSUE");
if (nativeIssueKeyPattern.test(legionIssue) || externalIssueRefPattern.test(legionIssue)) {
  return { args: { ...args, issue: legionIssue }, ref: null };
}
// only a bare number falls through to needing the cwd repository
const repo = await resolveCwdRepo(cwd, exec);
```

The regression tests added alongside it
(`packages/envoy-client/src/__tests__/dispatch-execute.test.ts:81`,
`packages/envoy-client/src/__tests__/dispatch-execute.test.ts:113`) assert against the two
forms `LEGION_ISSUE` can actually take in production (`"LEGION-3"`, `"owner/repo#42"`), not
just the bare-number form the original code assumed.

What actually caught the defect was neither suite: it was the coordinator's real-server
acceptance run at the end of the PR gate, calling the built executor with
`LEGION_ISSUE=sjawhar/legion#826` (the daemon's exact production shape, no `issue` argument
supplied) against a live Dispatch server and confirming the created issue's external ref
matched — recorded in the ledger as the live-stack proof on the merged head.

## Why This Works

A unit test can only check code against the input the test's author imagined. When two
packages communicate through an untyped channel (an environment variable is just a string —
there is no compiler checking the daemon's writer against the executor's reader), the
imagined input on each side can each be internally consistent and still not be each other's
actual input. Only a probe that uses the real producer's exact value crosses the seam.

## Prevention

- When a fix or a new capability spans a producer/consumer boundary carried by an untyped
  channel (env vars, JSON payload fields not covered by a shared schema, CLI flags), write at
  least one test — or, at minimum, one manual live-stack probe before merging — that feeds the
  *actual* value the real producer emits into the actual consumer, not a value the consumer's
  test author independently chose.
- Treat "both sides have green unit tests" as necessary, not sufficient, evidence for a change
  that touches an integration seam. The thermonuclear deep review that caught this ran against
  the real diff, not just the unit-test suite, precisely because unit tests do not exercise
  cross-package composition.

## Related Issues

- `sjawhar/legion#826`, whose thermonuclear deep review caught this and whose fix included
  regression tests for both real-world `LEGION_ISSUE` forms.
