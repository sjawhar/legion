---
title: "An invariant that forbids a string takes its exception from the fixture, not from the text"
category: testing
tags:
  - invariants
  - test-design
  - cli-output
date: 2026-11-01
status: active
module: packages/envoy/cmd/agent-secrets
related_issues:
  - "sjawhar/legion#2198"
---

# An invariant that forbids a string takes its exception from the fixture, not from the text

A test that says "no line of this output may name X" is one a later feature eventually has to
break, because one line legitimately names X. Weakening the list is how such a test dies.

- **Remove the one value the fixtures say is allowed, then check the whole forbidden list
  against what is left.** The exception is then exactly one string the test itself produced, and
  every other mention of X still fails.
- **Read that value from the fixture that answered it**, never by matching the output's text: a
  pattern that recognises "something shaped like the allowed value" excuses whatever else takes
  that shape.
- **Say in the test's own words which line is excepted and why**, and say the same at the site
  that prints it, so the next reader of either finds the other.
- **Name the residual.** The same value printed on another line also passes; write that down
  rather than implying the invariant is whole.

## Evidence

sjawhar/legion#2198 made a pending request's waiting line name the broker's `approve_url`, the
page a person must open, whose host is the broker's own.
`cmd/agent-secrets/source_test.go`'s `TestNoSessionLineNamesAHostOrAStoreID` forbids any line of
the command's own output naming a host, an account, a region or a project, and it went red with
eight subtests while passing on `main`.

The test now calls `approveURLOf`, which reads the `fakePending` fixtures for the exact
`approve_url` the fake broker answered the form with, removes that one string from the output,
and only then checks the absent list: the host anywhere else, `127.0.0.1` outside that URL, the
account id, the region, the project and "the broker at" are all still refused. The doc comment and
the comment at `waitingOn` each say which line is excepted and that the host is the one
`AGENT_SECRETS_URL` already names. The pull request's "Not proven / risk" records the residual:
that same URL printed on another line would pass.
