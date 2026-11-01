---
title: "A cache under a read the browser suite drives needs an off switch, not TTL zero"
category: testing
tags:
  - dispatch-e2e
  - playwright
  - cache
  - test-seams
  - dispatch-server
date: 2026-10-28
status: active
module: packages/dispatch/e2e
---

# A cache under a read the browser suite drives needs an off switch, not TTL zero

Extends docs/solutions/testing/a-test-only-knob-widens-the-value-production-runs-pin-it-at-the-construction-a-deployment-reaches-INFRA-56.md.

## Rules

- **Find the specs first.** Before you put a cache or a shared fetch under a Dispatch endpoint,
  grep `packages/dispatch/e2e/` for the helpers that change the fake behind it (for the Envoy
  listener: `setLiveSessions`, `seedAgents`, `setSessionLive`) and for the route itself. A worker
  runs against its own lane's server and reseeds that lane's fake, then navigates at once, so a
  copy taken before the reseed is shown for a whole poll interval.
- **Turn the cache off, do not shorten it.** A TTL of zero still lets reads that arrive during a
  fetch join it and get the answer from before the change. The switch makes every read call the
  dependency itself (the `uncached` branch of `readRouteRoster`, `api/issue_route_status.go`).
- **Ship it as a `DISPATCH_TEST_*` setting.** A browser test of the real binary cannot reach an
  unexported field, so that document's rule of an unexported seam does not fit here. Use the
  `DISPATCH_TEST_SETTLE_DELAY` pattern instead (`cmd/dispatch/main.go`): a settings row; boot
  refused unless `DISPATCH_TEST_HOOKS=1`; a `TestEverySettingReachesItsReader` case that proves
  the value reaches the router (`routerFor(resolveWith(…))`); and the variable set in both
  `packages/dispatch/e2e/run-server.sh` and
  `packages/envoy/deploy/compose/dispatch.acceptance.compose.yml`. The boot refusal is a new
  contract at a process boundary, so the pull request carries a `## Contract change census`.
- **Prove the switch is needed.** Run the specs you found with the switch, then once with its
  line taken out of `run-server.sh`; the second run must fail.

## Evidence

`DISPATCH_TEST_ROSTER_UNCACHED` on sjawhar/legion#2429: the 24 agents and `route_status` specs
passed with it (261 passed, 12 skipped); with the line removed, 7 of 60 tests in six agents specs
failed (PR comment 6450127431). The plan had only inferred the breakage until that run.
