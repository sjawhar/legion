---
title: "A key-namespace bug hides behind fixtures that use one string for two project names"
category: testing
tags:
  - fixtures
  - project-key
  - claim-token
  - event-lag
  - driven-proof
date: 2026-10-26
status: active
module: packages/daemon
related_issues:
  - "INFRA-225"
  - "sjawhar/legion#2188"
---

# A key-namespace bug hides behind fixtures that use one string for two project names

Extends docs/solutions/testing/fixtures-derive-what-production-derives.md.

- The Go daemon keys rows by one of two project names: the claim project token (lowercase,
  `claim.ProjectToken`, `api.Options.Project`) or the Dispatch project key (as `legion.yaml`
  writes it, `api.Options.DispatchProject`). They never match in a deployment. Name which one a
  new table is keyed by, pass both explicitly, and give the two different values in every
  harness, so a read with the wrong one finds nothing and fails.
- To prove an answer that must not depend on an event landing first (a write's own echo), hold
  the table that event writes in a lock for the length of the request
  (`lock table … in share mode`). On a bench the event usually wins the race; in a deployment it
  never does.

## Evidence

- `release_children` answered `held: []` in every deployment: `releaseOutcome` read the blocker
  mirror with the claim token while the mirror is keyed by the Dispatch key. The unit tests used
  one string for both. Only the driven proof against a real Dispatch caught it. `api/harness_test.go`
  now sets `testProject = "legion"` and `testDispatchProject = "LEGION"`.
- The driven proof's release step holds `dispatch_issues` in share mode while it releases two
  backlog children, so neither `todo` event lands before the answer. The answer read back from
  records failed there and passes when judged from the blockers Dispatch showed the request.
