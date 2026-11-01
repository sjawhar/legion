---
title: "A lease is a name, and the name is its blast radius"
category: architecture-patterns
tags:
  - advisory-lock
  - lease
  - multiprocess
  - blast-radius
date: 2026-10-23
status: active
module: packages/envoy/internal/dispatch/duty
related_issues:
  - "INFRA-66"
  - "sjawhar/legion#2087"
---

# A lease is a name, and the name is its blast radius

- Name a lease after the one resource whose work it serialises. A loop that reconciles N
  independent resources takes N leases, each named from its resource.
- Before accepting a lease that spans more than one resource, ask what a pass that **hangs** on
  behalf of one of them does to the others. Remember that a failed pass is usually not throttled,
  so its holder retakes the lease at once and the other resources get only the gaps between holds.
- A lease keyed by a hash (`pg_try_advisory_lock(ns, hashtext(name))`) has a collision mode that
  is silent and indistinguishable from correct operation: two names that collide become one
  lease, and both loops log the ordinary "skipped" line while mutually excluding each other for
  ever. Where the name set is derived rather than a list of constants, pin it with a test that
  collects every name the binary can take and asserts their hashes distinct.
- Mutual exclusion and once-per-period are two properties and need two mechanisms. A lease cannot
  make a *non-overlapping* pass count (two processes' tickers are out of phase by construction),
  and a record that a period was served cannot stop two concurrent passes (a pass spanning a
  boundary holds its lease while the boundary moves on). Say in the schema which carries which.

## Evidence

A change making one Dispatch server into N put both delivery durables' consumer reconciliation
under a single `delivery-consumers` lease. The merge with `main` then broke a live row main had
just added: `consumer delivery-jobs was not push-bound within 20s`.

Measured against a real `nats-server` with exactly one consumer-create permission denied: a denied
`AddConsumer` draws no reply, so it blocks the client's whole JetStream `MaxWait` — ten seconds —
**with the lease held**, and because a failed reconciliation is deliberately not throttled, the
failing intake retook the lease on its next two-second poll, back to back. The healthy durable's
intake could only create its own consumer in the milliseconds between holds, and in CI it never
did. One durable's misconfiguration was the other durable's outage.

The fix was the name: `consumerLease(durable)`, giving `delivery-consumer-delivery-events` and
`delivery-consumer-delivery-jobs`. In the same run the job intake bound in 1–8 ms while the events
create went on failing.

Two notes worth keeping with it:

- That fix reached the branch **inside a merge commit's conflict resolution**, where a behaviour
  change is invisible to anyone reading the commit list. See
  docs/solutions/github/a-pull-request-github-calls-mergeable-can-still-be-unmergeable-in-substance-INFRA-66.md.
- Making part of the name set derived (`consumerLease(durable)`) is what turned the hash-collision
  mode above from theoretical into something nobody can audit by eye; the only enumeration of the
  names in that tree is prose in a pool-size comment, and prose of exactly that kind had already
  gone stale once in the same review.

Related: docs/solutions/database-issues/a-cross-row-one-at-a-time-rule-in-a-where-predicate-is-write-skew-under-read-committed-lock-the-key-first-LEGION-694.md
(advisory-lock placement and namespacing, the neighbouring rule).
