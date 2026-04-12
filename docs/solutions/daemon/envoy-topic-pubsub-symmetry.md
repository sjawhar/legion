---
title: "Envoy Topic Publish/Subscribe Symmetry"
category: daemon
tags:
  - envoy
  - topic-routing
  - subscription
  - silent-failure
  - state-delta
date: 2026-04-12
status: active
module: daemon
related_issues:
  - "#454"
symptoms:
  - "controller not receiving state deltas"
  - "CI status changes not reaching controller"
  - "state delta notifications silently dropped"
  - "role-based topic delivery failing"
  - "publishStateDelta messages lost"
---

# Envoy Topic Publish/Subscribe Symmetry

Changing an Envoy publish topic without updating the corresponding subscription silently
breaks delivery. There is no compile-time enforcement — topic strings are inline in both
the publisher (`server.ts`) and subscriber (`index.ts`), and a mismatch produces zero errors.

## The Pattern

Envoy message delivery requires a **two-sided contract**:

1. **Publisher** calls `/v1/messages/publish` with a topic string
2. **Subscriber** registers interest in that topic via `/v1/interests/subscribe` or `/v1/roles/set`
3. Envoy's `registry.Match()` connects the two at delivery time

If either side changes independently, messages are published to a topic nobody listens on,
or a session listens to a topic nobody publishes to. Both cases are silent.

## Role Topics vs Explicit Subscriptions

| Mechanism | Registration | Failure mode |
|-----------|-------------|--------------|
| `notifications.role.<role>` | `/v1/roles/set` (claim) | Role claim is non-fatal (`console.warn`). If it fails, the topic vanishes from interests. |
| `notifications.<namespace>.<name>` | `/v1/interests/subscribe` (explicit) | Subscription is also non-fatal, but is a separate call that can be independently verified. |

**Use role topics** when the recipient is dynamic (multiple sessions might hold the role
at different times) and the publisher doesn't know the session ID.

**Use explicit named topics** when the recipient is a known, stable component. For
daemon→controller communication, the daemon already knows the controller's session ID and
sets up its subscriptions — explicit topics are more reliable because they don't depend on
the role claim succeeding.

## Verification Checklist

When changing any Envoy topic string:

1. **Grep both sides**: Search the repo for the topic string in both publish and subscribe contexts
2. **Check the subscription setup**: In `subscribeControllerToEnvoy()` (`index.ts`) for controller topics, or `subscribeWorkerToEnvoy()` for worker topics
3. **Check the publisher**: In `publishStateDelta()` (`server.ts`) or wherever the message originates
4. **Update tests**: Topic strings in test assertions are regression guards — update them to match

## Example: Issue #454

The daemon published state deltas to `notifications.role.legion-controller`. The controller's
role claim registered this topic in its interests. But the role claim is fire-and-forget with
`console.warn` on failure — if it fails silently, state deltas are dropped with no error.

Worker completion notifications (sent via `envoy_send` to the session ID directly) still
worked, masking the daemon-side delivery problem.

The fix changed the publish topic to `notifications.legion.controller` (explicit, non-role)
AND added it to the controller's subscription list. Both sides of the contract had to change.
