---
title: "A count of writes whose answer was lost is an upper bound, and each loss joins on the lost thing's own id"
category: best-practices
tags:
  - counting
  - reporting
  - idempotency
  - de-duplication
date: 2026-10-30
status: active
module: packages/pi-envoy
related_issues:
  - "INFRA-168"
  - "sjawhar/legion#2161"
---

# A count of writes whose answer was lost is an upper bound, and each loss joins on the lost thing's own id

When a report counts the writes a process could not confirm (failed, given up, cut at shutdown):

- **Name the figure for what it is:** the writes the writer could not confirm, an upper bound on what the server lacks. A write given up on the client's side may still land. Put that in the printed line, in the field's name and in any sentence that reconciles two reports ("the server's report counts at most that many fewer").
- **Give every written thing an id of its own, and join each failure record on that id.** Never join on the enclosing call's id when one call can produce several things. A failure joined on a shared id is copied onto every thing under it and counted once for each.
- **On the server side, drop duplicates by (writer, id) and count the copies on a line of their own.** That way, a write that was given up and landed anyway is counted once, and its second copy is visible.
- **Lock the join with a test that puts several things under one shared id,** together with one failure, and expects a count of 1.

## Evidence

- c0d1d13e (review thread `PRRT_kwDORFy7ds6x1893`): the session-file report copied a gated call's record loss onto every entry with the same `toolCallId`, the unmarked-write entries included. The reviewer's probe (one gated entry, two unmarked entries, one failure) counted 3. Each side now joins on its own key (`packages/pi-envoy/scripts/askgate-report.ts:288-299`), and an unmarked record carries `<toolCallId>#<line>` (`packages/pi-envoy/extensions/envoy.ts:3821`). The test 'a gated call's unconfirmed record is counted once, not again on each unmarked entry of its tool call' counts 3 on the old join.
- Thread `PRRT_kwDORFy7ds6x189-`: the figure was printed as `decision records lost` and described as decisions that never reached Dispatch. c0d1d13e renamed it `decision records not confirmed` and described it as an upper bound (`askgate-report.ts:26-32`).
- 984933ef gave each record an `id`, and the Dispatch reader keeps one record per (session, id) and prints `duplicate records: N`.
