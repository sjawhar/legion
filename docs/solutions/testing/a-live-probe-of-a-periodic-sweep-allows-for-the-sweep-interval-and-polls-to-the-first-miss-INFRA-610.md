---
title: "A live probe of a periodic sweep allows for the sweep interval and polls to the first miss"
category: testing
tags:
  - live-proof
  - reaper
  - negative-control
  - envoy
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-610"
  - "sjawhar/legion#2374"
---

# A live probe of a periodic sweep allows for the sweep interval and polls to the first miss

When you prove from outside that a sweep running every `T` changed its deadline from `D` to `D′`
(`D′ > D`):

- The old build deletes anywhere in `(D, D+T]` after the last write, so it can still answer
  "present" up to `D+T`. Call the new rule only when the last "present" comes after `D+T`. Call the
  old rule only when the first "absent" comes clearly before `D+T`. Anything else is inconclusive:
  run again. Do not set the new-rule floor at `D′` minus a margin.
- Poll every few seconds until the first "absent", and record the largest age seen present (A) and
  the smallest age seen absent (B). Two fixed checks ("present at 14:45, absent at 21:00") cannot
  tell the two rules apart.
- Start two subjects less than one interval apart, so no single sweep tick can decide both.
  Start the probe just before a tick, so each deletion tick falls outside the band where both
  rules give the same answer.
- Count a 5xx answer or a timeout as unknown, never as absent. Measure clock skew from the
  server's own timestamp in the write's answer.
- Run the same probe against a build of the base commit as the negative control. It must read
  "old rule".

For Envoy's interest reaper, reuse dispatch://INFRA-610/artifact/reap-probe-sh. A subscribe with no
`port` and no `self_subscribed` writes only the interest row (`packages/envoy/cmd/listener/api.go:1272`
gates the session-registry write), so the reaper always sees that session as dead and the row's
`updated_at` is the only clock in play.

## Evidence

- The reaper ticks every 5 minutes from listener start
  (`packages/envoy/cmd/listener/main.go:938`). Under the old 10-minute rule a row is deleted
  between 10:00 and 15:00, and under the new 15-minute rule between 15:00 and 20:00.
- The plan's verdict was "new rule when A >= 14:50". A run against the base listener read A 14:50.0
  and B 14:54.9 (a tick at age 14:50.3), which that verdict would have called "new rule". The
  probe's verdict became A >= 15:01, and the header of dispatch://INFRA-610/artifact/reap-probe-sh
  records why.
- Started 60 s before a tick, the head listener read `-a` A 15:54.9 / B 15:59.9 and `-b`
  A 18:24.9 / B 18:29.9 (new rule). The base listener read `-a` B 11:00.0 and `-b` B 13:29.9
  (old rule). On the head listener, the tick at row age 10:59.9 reaped nothing.
