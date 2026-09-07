---
title: "NATS `>` Matches One or More Tokens, Not Zero"
category: envoy
tags:
  - envoy
  - nats
  - routing
  - subscriptions
date: 2026-09-07
status: active
module: envoy
symptoms:
  - "a session subscribed to `pr.<n>.>` receives comments and checks but never the PR's own opened/synchronize/closed events"
  - "a Go routing test passes for a pattern that a native NATS subscriber never matches"
---

# NATS `>` Matches One or More Tokens, Not Zero

## Symptom

Agents were told to subscribe to `notifications.github.<owner>.<repo>.pr.<n>.>` as the one
subscription for a PR. They received `pr.<n>.comment`, `pr.<n>.review`, and `pr.<n>.checks`,
but never the lifecycle events published on the base subject `pr.<n>` itself — `synchronize`
with the new head sha, `closed` with `merged: true`. Sessions learned of merges from refused
pushes.

## Mechanism

In NATS, the `>` wildcard matches **one or more** trailing tokens. `a.b.>` matches `a.b.c` and
`a.b.c.d` but never `a.b`. Envoy's Go matcher (`internal/routing/match.go`) treated `>` as
zero-or-more, so the listener's fan-out path matched `pr.42` for a `pr.42.>` interest — but
pi-envoy and the Claude bridge subscribe to NATS natively, where the real semantics apply. The Go
unit tests encoded the wrong rule and passed.

## Fix

Two parts, both shipped together:

1. `routing.Match` now implements NATS semantics (`>` requires at least one token; `*` exactly
   one), with tests asserting `pr.42.>` does **not** match `pr.42`.
2. `envoy_subscribe` expands a requested topic of the form `<concrete-subject>.>` to register the
   base subject too. Callers keep writing one topic; the registry holds two rows. Unsubscribing
   the wildcard removes both. Bases containing `*`, `>`, empty segments, or a trailing dot are not
   expanded (the first two are not concrete; the last two are malformed and rejected).

The tool guide and `skills/envoy/SKILL.md` state the rule plainly so nobody re-derives it.

## Lesson

When a matcher exists in more than one runtime (Go fan-out, native NATS in two TypeScript
hosts), the unit test for the Go copy is not evidence about the others. Test a routing rule
against the broker's own semantics, or against a fixture that the native subscriber path
consumes.
