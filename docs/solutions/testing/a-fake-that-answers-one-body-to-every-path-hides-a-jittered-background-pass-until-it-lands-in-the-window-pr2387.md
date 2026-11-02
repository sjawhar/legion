---
title: "A fake that answers one body to every path hides a jittered background pass until it lands in the window"
category: testing
tags:
  - test-fake
  - flaky-test
  - jitter
  - background-loop
  - multiprocess
date: 2026-11-01
status: active
module: packages/envoy/cmd/dispatch
related_issues:
  - "sjawhar/legion#2387"
---

# A fake that answers one body to every path hides a jittered background pass until it lands in the window

Extends docs/solutions/testing/a-random-delay-added-to-a-shared-helper-is-injectable-and-pinned-in-every-test-that-does-not-test-it-pr2323.md.

That note pins a random draw a test does not test. This one is for a background pass whose first
run is drawn at random over minutes, inside a real process the test starts, against a fake that
answers the same body to every request.

- **A catch-all answer is a latent failure, not a neutral one.** A fake that answers `[]` (or
  `{}`) to every path serves the call the test was written for, and also every call a background
  loop makes. The loop's first pass lands inside the test's window only now and then, so the wrong
  shape surfaces as a rare flake far from its cause. Route the fake by path and answer each one
  in the real service's shape, and give an unknown path the service's own 404, so an unexpected
  call fails loudly rather than decoding garbage.
- **Force the pass into the window, never wait it out.** Add a test setting that runs the loop's
  first pass as the process starts, gated like the other test-only settings (refused without the
  test-hooks switch), and covered by the test that checks every setting reaches its reader. A
  sleep long enough to cover the jitter would make the row minutes long and still prove nothing on
  a slow runner.
- **Wait for the pass to end, then read every process.** Wait on the line the lease or loop logs
  after the pass returns, then check every process's log for errors, not only the process the row
  is about: the pass runs on whichever process takes its lease.
- **Prove it fails first.** Run the forced pass with the old fake and see the error the flake
  showed. Then answer each step in turn: a pass reads a sequence of endpoints, and the second
  wrong shape only shows once the first is fixed.

## Evidence

In sjawhar/legion#2387, CI run 41293708339 failed `TestOneProcessBindsEachDeliveryDurableAndTheOtherStandsBy`:
the standby logged `GitHub App permission check ... decode installation: json: cannot unmarshal array`.
`startGitHubFake` (`packages/envoy/cmd/dispatch/multiprocess_test.go`) answered `[]` to every
path, and the delivery reconcile's first pass, jittered over five minutes (`duty.RunJittered`,
`delivery/reconcile.go`), had landed about three seconds after boot. With
`DISPATCH_TEST_RECONCILE_AT_START=1` (`delivery.Reconcile.FirstPassAtStart`, refused without
`DISPATCH_TEST_HOOKS=1`) the old fake failed on the first run with that error. With the
installation fixed, it failed again on `list installation repositories`. With every step answered
(installation object, token mint, repository list, GraphQL search, run and job listings, 404
otherwise) it passed 20 of 20 under `-tags multiprocess`.
