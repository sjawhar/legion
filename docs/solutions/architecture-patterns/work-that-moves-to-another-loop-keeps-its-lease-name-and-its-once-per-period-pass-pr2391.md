---
title: "Work that moves to another loop keeps its lease name and its once-per-period pass"
category: architecture-patterns
tags:
  - lease
  - duty
  - rolling-deploy
  - forward-merge
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2391"
---

# Work that moves to another loop keeps its lease name and its once-per-period pass

Extends docs/solutions/architecture-patterns/a-lease-is-a-name-and-the-name-is-its-blast-radius-INFRA-66.md.

- When you move a piece of background work out of one loop into another, keep the lease name the
  work already runs under, even when the new loop's own name would read better. During a rolling
  deploy the previous build still runs the work under the old name, and only a shared name makes
  the two builds exclude each other on it.
- Work that must run once per period across the deployment (a key that expires by the clock, a
  delivery a dead process left owed) stays on a leased once-per-period timer
  (`duty.RunLeased`). Never fold it into a woken loop. A woken loop holds its pace per process, so
  N processes woken by one change run per-deployment work N times.
- After a forward merge that brings main's work into a loop your branch changed, count that work's
  statements per process at idle, against the fork point. A merge that compiles and passes its
  tests can still double how often the work runs.

## Evidence

Pull request sjawhar/legion#2391. Merging main's #2449 put the Idempotency-Key sweep and the
owed-delivery resume inside the branch's woken suggestion loop, which takes no lease. With two
processes, the idle measurement at 4444ead7 counted 60 key sweeps and 80 resumes in 10 minutes,
against 30 and 40 at the fork point. Commit 141face2901b moved both to `api.RunMaintenanceSweep`
(`packages/envoy/internal/dispatch/api/maintenance.go`), a leased one-minute timer that keeps the
lease name `suggestion-outcomes` (`maintenanceLease`, lines 19-24 there). At 141face2 the counts
were 30 and 40 again.
