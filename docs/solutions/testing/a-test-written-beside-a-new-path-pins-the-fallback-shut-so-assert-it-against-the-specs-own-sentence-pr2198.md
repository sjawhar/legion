---
title: "A test written beside a new path pins the fallback shut, so assert the fallback against the spec's own sentence"
category: testing
tags:
  - rollout
  - backward-compatibility
  - test-design
  - e2e
date: 2026-11-01
status: active
module: packages/dispatch
related_issues:
  - "sjawhar/legion#2198"
---

# A test written beside a new path pins the fallback shut, so assert the fallback against the spec's own sentence

An expand step keeps working against a peer that has not deployed yet. The compatibility half is
routine; the test half is where it goes wrong, because the tests written alongside the new path
describe what the new code does, and a fallback that quietly stopped working then has a passing
test of its own.

- **Write the fallback's assertion from the spec's sentence, not from the code's behaviour.**
  "Keeps today's flow" names a link, a page, a line a person sees; assert that, and the test
  fails when the new path swallowed it.
- **A test that reads the degraded case is not proof it still works**: it is proof of whatever
  the code does there, which is the thing under suspicion.
- **The deploy order is what the fallback protects**, so each fallback owes the step that
  depends on it a named test, and the pull request body should say which test is which step's.

## Evidence

In sjawhar/legion#2198, rollout step 1 made a pending secret request's Inbox row in Dispatch a
link to the request's page on the broker, and the spec requires that against a broker from before
that page — the whole of step 2's safety — the row keeps linking the record's own Dispatch page.
It did not: `CredentialRequestsSection.tsx` rendered an unlinked `<div>` when the broker's answer
named no `approve_url`. Two tests covered that case and both pinned the wrong behaviour, a unit
test in `CredentialRequestsSection.test.tsx` and a browser test in `inbox.e2e.ts`, each asserting
the text the row showed rather than the link the spec requires. The review's one Critical finding
is that pair.

The fixed browser test reads the row as a link with `href="/credentials/record-old-broker"`,
counted in "Needs you 1", and the same round gave `agent-secrets`' waiting line its own fixtures
for a broker that names no page and one that names no record at all.
