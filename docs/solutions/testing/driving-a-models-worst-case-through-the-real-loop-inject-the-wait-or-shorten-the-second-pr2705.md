---
title: "Driving a model's worst case through the real loop: inject the wait, or shorten the second"
category: testing
tags:
  - go
  - clocks
  - timing-tests
  - flake
date: 2026-11-04
status: active
module: packages/envoy/internal/broker/leasetime
related_issues:
  - "sjawhar/legion#2705"
---

# Driving a model's worst case through the real loop: inject the wait, or shorten the second

Extends docs/solutions/testing/a-test-of-a-margin-stands-off-its-boundary-and-reads-the-margin-from-the-code-pr2641.md.

That page's rule — stand strictly inside and outside, and read the margin from the code — holds
here too. What is new is a model that predicts a real loop's timing, and the two shapes a row
that replays the model's worst case through that loop can take.

- **Pick the shape by whether the loop's wait is injectable.** A loop that takes its wait as a
  function field (`after func(time.Duration) <-chan time.Time`) gets a virtual clock: the rig
  advances by exactly the duration asked for and returns at once, so a twenty-minute outage runs
  in microseconds and no scheduler can change the verdict. A loop that calls `time.After` and
  `time.Until` itself cannot, and the row runs the whole scenario at a shortened second instead,
  with every figure of the model scaled by the same one factor.
- **Choose the shortened unit from a measured flake rate, not from how fast you want the row.**
  Run the row a hundred times under the load the machine really carries — the rest of the module's
  suites beside it — and raise the unit until it stops disagreeing with the model. Record the
  rejected unit and what it did in the comment, so the next person does not shorten it again.
- **A real-time rig cannot end a gap on the instant a renewal goes out.** Hold the gap open a
  margin past the model's instant, and assert that margin against the phase the model produced,
  so a row whose derived case is degenerate fails instead of passing vacuously.
- **Keep the exact boundary in the pure-model test and the loop rows a measured margin away.** At
  the floor the model's attempt lands nanoseconds inside the lease, and a rig that spends
  milliseconds signing a proof cannot tell that from a lapse. The model pins the boundary; the
  loop proves the loop walks the model's schedule.
- **Both sides of the boundary, every time:** one case the model says lapses and one it says
  survives, through the same loop.

## Evidence

`sjawhar/legion#2705` (dispatch://INFRA-454) has one of each, for the two renewers of the secrets
broker.

Injectable: `cmd/agent-secrets/renew_outage_test.go`'s `outageRig.after` is `renewLoop`'s wait —
"it advances the renewer's clock and returns at once … Nothing here sleeps, so the whole outage
runs in microseconds of real time". The row drives the model's worst phase, including the partial
failure delays, and asserts the first wait the loop asked for against `leasetime.RenewAfter`.

Not injectable: `internal/broker/helper/renew_outage_test.go` states why and what it costs —
"`renewLoop` has no clock of its own to inject — it waits on `time.After` and reads
`time.Until(lease)` — so the row runs the whole outage at a shortened second instead, with every
wait of the model shortened by the same factor (`leasetime.Model.Scale`)". `outageUnit` is
`10 * time.Millisecond` and `minMargin` is `40 * outageUnit`, and the comment keeps the measurement
that set them: "It was 2 ms once, and at that unit one run of this row in a hundred, taken while
every other Postgres suite of this module ran beside it, launched one renewal more than the model
walked." The gap is held 100 ms past the model's instant, with that slack asserted against the
phase.

The margin rule is load-bearing: the floors themselves (91 s at a 1 s sweep, 105 s at 10 s, 360 s
at 60 s) are pinned in `leasetime_test.go`, where a renewal landing one nanosecond inside its
lease is an exact comparison, and the loop rows run at leases well clear of them.
