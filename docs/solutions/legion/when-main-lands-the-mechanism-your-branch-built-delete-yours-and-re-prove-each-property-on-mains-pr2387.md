---
title: "When main lands the mechanism your branch built, delete yours and re-prove each property on main's"
category: legion
tags:
  - forward-merge
  - cutover
  - duplicate-loop
  - lease-pool
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2387"
---

# When main lands the mechanism your branch built, delete yours and re-prove each property on main's

Extends docs/solutions/testing/when-a-sibling-merge-takes-the-measured-gain-judge-each-part-on-evidence-of-its-own-pr2497.md.

That note is about a sibling taking a measured performance gain. This is a behavioural mechanism
both sides built.

- At each forward merge, read main's new background loops and senders against your own. When one of
  main's does the job one of yours does, raise it with whoever owns the scope; the boring answer is
  to delete yours and attach your senders to main's, since two loops for one job is duplication and
  two leases for it a budget spent twice.
- List every property your mechanism guaranteed and show each on main's, test by test: main's was
  not written for your invariants. Name each parameter that differs (a horizon, what a resend
  carries, what is settled instead of sent) and the user-visible change it makes; carry the ones
  your change requires into main's, and disclose the rest on the pull request.
- Recompute the host loop's worst-case pass now that your per-item work runs inside it, and how long
  it holds its lease.
- Give back the budgets yours claimed (a lease-pool size, a connections-per-pod constant) so they
  show no diff against main, and remove every setting, comment and document that named yours.
- Move your tests onto main's mechanism at production's own interval, and show each fails with main's
  mechanism switched off.

## Evidence

This branch's `RunDeliveryResume` (a leased loop, one-hour horizon) and main's
`resumeOwedDeliveries` (`packages/envoy/internal/dispatch/api/delivery_sweep.go`, run from the
suggestion-outcome sweep, 72 h window) resent the same stranded attempts. The branch deleted its
loop, its `delivery-resume` lease and `DISPATCH_TEST_DELIVERY_RESUME_INTERVAL`; `store.leasePoolSize`
and `infra/gcp`'s `dispatchConnectionsPerPod` returned to main's values. Main's sweep had sent no
urgency; the claim now reads it from the attempt row
(`TestTheDeliverySweepResumesAnAttemptWithTheUrgencyItRecorded`), and the 72 h window replaced the
one-hour horizon as a disclosed change. The kill rows in `packages/envoy/cmd/dispatch/multiprocess_test.go`
pass at main's one-minute sweep with no interval override (about 68 s each) and read `[1:pending]`
after 2m30s with the sweep switched off. Pull request sjawhar/legion#2387.
