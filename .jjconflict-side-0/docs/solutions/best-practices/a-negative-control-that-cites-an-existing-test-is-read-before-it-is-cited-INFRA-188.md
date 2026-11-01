---
title: "A negative control that cites an existing test is read before it is cited"
category: best-practices
tags:
  - evidence
  - negative-control
  - test-coverage
  - dispatch-tools
date: 2026-10-25
status: active
module: packages/contracts/src/dispatch-tools.test.ts, packages/envoy-client/src/__tests__/dispatch-execute.test.ts
related_issues:
  - "INFRA-188"
  - "sjawhar/legion#2174"
---

# A negative control that cites an existing test is read before it is cited

Extends docs/solutions/best-practices/a-statement-about-evidence-is-held-to-the-standard-of-the-evidence-it-describes-INFRA-71.md.
That document holds a check you ran to the standard of the fault it claims to catch. The delta is
the control you did not run: a proof row whose negative control is *an existing test, by
`file:line`, that someone else wrote*. Opening it costs one read, and the citation is a claim
about coverage until you do.

- Read the cited test's body, not its name or its line number. A test named for a tool often
  asserts over one tool only, and the loop you assumed is a single `find`.
- When the read shows the coverage is absent, say so in the handoff and name the test that would
  close it, rather than quietly citing a weaker control. The next change to that shape is the one
  that lands the test.
- An assertion that something is *absent* can pass vacuously. Pair it with an assertion that the
  call happened at all — the recorded request's count, or a named capture per call — so that a
  reordering of the cases cannot leave the absence assertion reading nothing.

## Evidence (INFRA-188, sjawhar/legion#2174)

The change added two arguments to the `dispatch_issue_update` tool spec and, in the first review
round, updated the spec's `example`, which `dispatch <command> --help` and every refusal print as
a command line (`packages/contracts/AGENTS.md:62`, `packages/envoy-client/src/tool-input-errors.ts`
`exampleFor`). The implementer's handoff gave as that row's negative control:
"`packages/contracts/src/dispatch-tools.test.ts:298` parses every spec's example against its own
schema; an example that did not fit the argument shape fails there."

The retro's fresh-eyes read opened that line. The test is
`dispatch_issues requires a project and accepts only a nonnegative integer offset`
(`dispatch-tools.test.ts:294-298`), and its `spec` is
`dispatchToolSpecs.find((candidate) => candidate.name === "dispatch_issues")` — one tool. The only
other specs whose `example` any test reads are `dispatch_ask` and `dispatch_edit_ask` (`:78-79`);
`validCalls.dispatch_issue_update` (`:76`) is a hand-written object, not the spec's example. A
schema-invalid `dispatch_issue_update` example would have shipped green, and the cited control
could not have failed. The gap the next argument-shape change should close is one loop over
`dispatchToolSpecs` asserting each spec's example parses under its own schema; note its limit
before relying on it — it proves the example parses, never that it illustrates the new argument,
which is what the review round actually found missing.

The second instance is in this change's own new test. After the three move shapes,
`expect(bodies.at(-1)).not.toHaveProperty("rank")`
(`packages/envoy-client/src/__tests__/dispatch-execute.test.ts:2896-2897`) asserts a status-only
call sends no `rank` key. It is sound only because three earlier calls in the same test already
pushed bodies. Move the status-only case to the front — a plausible future edit — and
`bodies.at(-1)` is `undefined`, the assertion passes against nothing, and the regression the test
exists for goes unseen.
