---
title: "A Dispatch route whose body travels as a JSON string caps its request at six times the body, and takes that limit and its 413 from routeBody"
category: envoy
tags:
  - dispatch
  - request-body
  - idempotency-key
  - body-drain
  - "413"
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2354"
---

# A Dispatch route whose body travels as a JSON string caps its request at six times the body, and takes that limit and its 413 from routeBody

- **Size the request from the escaped wire form, not from the decoded cap.** A body that a client
  sends as a JSON string can grow to six bytes on the wire for each byte of the decoded text
  (`<` sent as `\u003c`, a control character as `\u0001`). Set the route's request limit to six
  times the decoded cap plus room for the other fields, and check the cap itself on the decoded
  value.
- **Give that limit and its 413 one source.** Three layers read a request body before or around
  the handler: the drain that waits for a body before an early answer (`body_drain.go`), the
  keyed-write layer that spools and replays an `Idempotency-Key` request (`request_keys.go`), and
  the handler's own decode. Each one answers a body past the limit. Add the route as a case of
  `routeBody(method, pattern) (limit int64, tooLarge *apiError)` in `request_keys.go`, and have the
  handler pass both values to `decodeJSONWithin`. A route that is not a case there gets the 1 MiB
  JSON default and `413 REQUEST_TOO_LARGE` from the drain and the keyed layer, whatever its
  handler says.
- **Test each layer at that limit**, so removing the route's case from `routeBody` fails a test:
  an early answer past the limit (the drain answers the route's own 413, having read the limit
  and one byte more), a keyed repeat above 1 MiB and inside the limit (`409 KEY_IN_FLIGHT` once
  the whole body has arrived), a keyed write above 1 MiB that spills to a temporary file and
  replays, and the same key with another body (`422 KEY_REUSED`).

## Evidence

sjawhar/legion#2354 added `POST /api/v1/issues/{key}/handoffs/{phase}`, whose body is one JSON
object of at most `contracts.DispatchHandoffBodyMax` (262,144) bytes carried as a string.
`handoffRequestLimit` is `6*contracts.DispatchHandoffBodyMax + 4<<10`, 1,576,960 bytes, with the
escaping reason in its comment in `packages/envoy/internal/dispatch/api/handoffs.go`. A body of
262,145 bytes is refused by the `dispatch` command before it sends and by the server as
`413 CAP_EXCEEDED`.

Main's keyed-write layer (#2449) arrived in a forward merge. The first resolution put the handoff
write's limit in two layers (`routeBodyLimit`, `bodyTooLarge`) and remapped the 413 a third time in
`createHandoff`. Review round 11 found that deleting either branch passed every test. Commit
96f3c40b made `routeBody` the one source, deleted the remap, and added
`TestAnEarlyHandoffAnswerDrainsUpToTheHandoffWritesLimit`,
`TestAKeyedHandoffWritePastAMegabyteSpillsAndReplays` and the `KEY_REUSED` case of
`TestARetriedHandoffWriteReplaysItsAnswerAndRecordsOneVersion`. With `routeBody`'s handoff-write
case removed, all three fail with `413 REQUEST_TOO_LARGE` at 1,048,576 bytes.
