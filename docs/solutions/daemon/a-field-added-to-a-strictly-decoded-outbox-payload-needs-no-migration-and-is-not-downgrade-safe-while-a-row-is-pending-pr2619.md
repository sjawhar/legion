---
title: "A field added to a strictly decoded outbox payload needs no migration and is not downgrade-safe while a row is pending"
category: daemon
tags:
  - outbox
  - payload
  - rollback
  - DisallowUnknownFields
  - dedupe
date: 2026-11-01
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2619"
---

# A field added to a strictly decoded outbox payload needs no migration and is not downgrade-safe while a row is pending

Extends docs/solutions/daemon/plugin-daemon-api-contract-version-gate.md.
That note is a strict schema refusing an unknown field on an API response, which is read once.
A durable queued row is retried, so the same refusal holds the queue.

The outbox stores each row's payload as JSON and decodes it with `DisallowUnknownFields`
(`record.DecodeOutboxPayload`). When you add a field to a payload type:

- **No schema migration is needed, and a JSON predicate is the right dedupe.** A pending-row check
  of the form `select exists (select 1 from outbox where kind = $1 and issue = $2 and
  payload->>'<field>' = 'true')`, as `PersonHandoverPending` (`record/store.go`) is, reads the new field from the
  payload column.
- **State the rollback hazard in the pull request's risk section.** A daemon of the release before
  the field cannot decode a row that carries it; the row fails, retries on the lane's backoff, and
  holds that issue's later rows of the same lane behind it (the lane runs an issue's rows in order).
  The window is the seconds a row of the new kind is pending at the rollback. Say so in the deploy
  note rather than widening the decoder, which would let a misspelled field pass silently.
- **Mark a new row kind with a field, not a new `OutboxKind`,** when the executor is the one that
  already runs the row: a kind needs a lane entry, an executor arm and a finish path, and
  `TestEveryOutboxKindIsInExactlyOneLane` pins the table; a field rides the existing kind and the
  executor reads it where its guard belongs.

## Evidence

sjawhar/legion#2619 (LEGION-755) adds `StatusWrite.PersonHandover`
(`packages/daemon/internal/record/outbox.go:122`, `json:"personHandover,omitempty"`), read by
`record.Postgres.PersonHandoverPending` (`record/store.go:1127`) and by the status executor's
label guard (`packages/daemon/internal/daemon/outbox.go:522`). The decoder's strictness is
`record/outbox.go:558` (`decoder.DisallowUnknownFields()`). The reviewer's approval of
sjawhar/legion#2619 (review 5726715630, its inline note at `record/outbox.go:122`) names the
rollback window and the held later rows, and the pull request body's "Not proven / risk" carries
it as the deploy note; the plan (D1) chose the field over a kind so the existing `dispatch_status`
executor delivers the row.
