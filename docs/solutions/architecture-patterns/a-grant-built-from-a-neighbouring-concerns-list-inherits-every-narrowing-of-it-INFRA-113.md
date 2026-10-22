---
title: "A grant built from a neighbouring concern's list inherits every narrowing of it"
category: architecture-patterns
tags:
  - authorization
  - nats
  - permissions
  - coupling
  - deployment
date: 2026-10-22
status: active
module: envoy
related_issues:
  - "INFRA-113"
  - "sjawhar/legion#2105"
---

# A grant built from a neighbouring concern's list inherits every narrowing of it

Build a grant from the namespace boundary it is allowed to reach, never from an enumeration of that
namespace's current members. A list written to answer one question — what does this stream retain —
answers an authorization question wrongly the moment somebody edits it for the first question's own
reasons.

Pair every hand-written grant fixture with one live assertion that the rendered grant admits what
the code actually sends. A fixture that spells out the rendered list proves rendering fidelity and
can never ask whether the grant still covers the caller; it goes green again as soon as whoever
narrowed the grant updates the literal.

When two sides of one namespace are built from different sources they will disagree. Write both as
intent, or derive both from the same place.

## Evidence

`core.ListenerPermissions` (`packages/envoy/infra/core/permissions.go`) renders the NATS publish
grant of Envoy's listener into the hub's authorization. Until INFRA-113 it opened:

```go
publish := append(bus.StreamSubjects(),
    contracts.RoleTopicPrefix+">",
    bus.ExceptionTopicPrefix+bus.NotificationTopicPrefix+">",
    …)
```

`bus.StreamSubjects()` exists to answer what the `ENVOY_NOTIFICATIONS` stream retains
(`packages/envoy/internal/bus/stream.go`). In August 2026 that list was narrowed from
`notifications.>` to seven named prefixes — a retention decision, made in a change that never
opened `permissions.go`. Two months later the production hub was built from the narrowed grant,
and every publish to a topic outside those prefixes was denied by the server:
`Publish Violation - Subject "notifications.team.deploy-squad.all"`, with the caller left waiting
out a five-second acknowledgement deadline and answering `500 context deadline exceeded`. No
session could publish to a topic it named itself for six days.

The fix is one line stating the intent the grant always had:
`publish := []string{bus.NotificationTopicPrefix + ">", …}`.

The asymmetry is the tell. `SessionPermissions`' **subscribe** side was already the wide
`notifications.>`, because nobody derived it from anything. One side of the same subject space was
written as intent and stayed correct for two years; the other was written as a derivation and
rotted in two months.

`permissions_test.go` hand-writes the expected publish list — "written out by hand from Envoy's
names as they stand". When the stream list narrowed, that test went red exactly once and was
updated with the narrowing. It records a change; it never asks a question. The guard that would
have caught the defect is a relation — for every topic the publish path can emit, the rendered
grant admits it — and in this repository it exists only as a live test
(`packages/envoy/infra/core/permissions_live_test.go`, which publishes an ad-hoc
`notifications.team.*` topic through the real listener under the rendered authorization and
requires a credential-free subscriber to receive it). It needs Docker, so it runs in CI's
`envoy-infra-go` job and in no issue pod.

## When to apply

Any `[]string` of subjects, paths, buckets, scopes or topics that one concern owns and another
reads. Before adding the second reader, ask what the second reader would do if the list were
narrowed for the first reader's reasons; if the answer is "quietly refuse traffic", the second
reader wants the boundary, not the list.

## Related

- `docs/solutions/testing/a-golden-fixture-is-not-the-deployment-cite-the-live-assertion-for-a-live-fact-INFRA-69.md`
  — a golden is authoritative for the renderer and silent about the deployment.
- `docs/solutions/testing/fixtures-derive-what-production-derives.md` — the mirror image: a test
  helper hardcoding what production derives.
- `docs/solutions/envoy/a-list-that-defines-one-concern-acquires-readers-in-others-INFRA-113.md` —
  the same list's second defect, in routing rather than authorization.
