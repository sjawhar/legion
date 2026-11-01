---
title: "An ad-hoc notification topic's transport and reach are judged from the binary's compiled stream subjects, and a grant other grants compose widens every user that composes it"
category: envoy
tags:
  - nats
  - jetstream
  - ad-hoc-topics
  - permissions
  - delivery
date: 2026-10-22
status: active
module: envoy
related_issues:
  - "INFRA-117"
  - "sjawhar/legion#2104"
  - "INFRA-122"
---

# An ad-hoc notification topic's transport and reach are judged from the binary's compiled stream subjects

Two measurements to take before claiming what a change to the notification transport or to the
listener's grant does.

**1. The capture test reads the subjects this binary compiled, not the stream the server runs.**
`streamCaptures` walks `streamSubjects` in the running build, so a topic that a *newer* build's
stream subject would capture goes over core NATS from an *older* build: the deployed stream still
stores it (stream-ensure only adds subjects —
`docs/solutions/envoy/nats-jetstream-stream-ensure-only-adds-subjects.md`), but without a MsgId,
and the older listener's durable never fans it out. Where the grant and the listener image roll
from different sources — the hub renders its NATS authorization from a side branch and runs a
published listener image — the two must ship in one update, image first if one must lead.

**2. A core-NATS topic reaches only a session that holds its own NATS connection.** A session
registered with a port and `driving: true` takes its deliveries as HTTP prompts the listener
posts from its *durable JetStream consumer*, whose `filter_subjects` are the compiled stream
subjects. An ad-hoc topic never enters the stream, so it reaches no such session, however green
the publish is. Measure the host class, not the publish's status code: `200` means the server
confirmed the publish, never that the intended reader received it.

**3. A permission constructor other constructors compose widens every user that composes it.**
Before accepting an acceptance line that names one principal, read the callers of the constructor
you edit: here `ListenerPermissions` is unioned into `HostPermissions` and reached by
`DispatchPermissions`, so one line widened three rendered users. The rendered cloud-config
goldens are the census that shows the real blast radius, and they are the cheapest place to see
it (`cd packages/envoy/infra && go test ./gcp/ -skip 'DataDisk'`).

## Evidence

INFRA-117 granted the hub's listener user `notifications.>` and routed every `notifications.`
topic the stream does not capture over confirmed core NATS.

- All three rendered users changed in `packages/envoy/infra/gcp/testdata/cloud-config.golden`
  (`example-host-a`, `example-dispatch`, `example-hub`), though the spec's acceptance line named
  the hub's listener user alone; the planner raised it and the architect corrected the spec.
- On a live rig the tester registered a session with a port and `driving: true` for both an
  ad-hoc topic and a captured one: the captured topic arrived as
  `POST /session/<id>/prompt_async` in about 50 ms, the ad-hoc topic never arrived within 4 s, and
  both publishes answered `200`. A session on its own NATS connection received both. That gap is
  filed as dispatch://INFRA-122; it is not a regression, since before the fix the publish failed
  for every host.
- The `>` and `*` tokens are literal here: `notifications.>` as a *topic* passes the listener's
  prefix refusal and is delivered only to a subscriber whose pattern matches that literal token,
  never fanned out to subscribers of concrete topics.
- A core publish the client could not confirm can still land: with the server frozen
  (`SIGSTOP`) the publish answered `500` on a flush timeout, and the buffered message was
  delivered once the server resumed. The contract is "not known to have been accepted", never
  "known not to have landed".
