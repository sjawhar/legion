---
title: "A frame from another binary is modelled from that binary's own schema and pinned with a line that binary wrote"
category: testing
tags:
  - wire-format
  - fixtures
  - strict-decoder
  - pinned-binary
date: 2026-10-31
status: active
module: packages/daemon/internal/shimwire
related_issues:
  - "sjawhar/legion#2571"
---

# A frame from another binary is modelled from that binary's own schema and pinned with a line that binary wrote

Extends docs/solutions/testing/integration-seam-needs-producer-exact-value.md with the failure
mode a strict decoder adds, and with how to get the producer's real line when the field only
appears on a path that is hard to reach.

A hand-written fixture of another process's frame is a guess about that process. Tests built on it
are green against a shape the producer never emits, and they stay green for ever, because both
sides of the test are yours.

- Read the producing binary's **own schema** for the frame — its declaration, not its happy-path
  output — and model every member, including the ones you do not read. A decoder that keeps only
  today's reader silently drops the rest of a failure the next reader will want.
- Pin the decode with a line **the binary wrote**, in a test that says so, beside the hand-written
  table.
- When the field only appears on an error path, make the producer take it: a stand-in server for
  its upstream, answering the refusal that path needs. **Choose a refusal the producer does not
  retry.** A retryable status sends it into its own backoff, and the frame arrives minutes later
  or not at all.
- With a strict decoder, the old shape is its own negative control: feed the real line with the
  field as the type the code declared before, and the refusal you see is the frame the daemon was
  dropping.

Know what a dropped frame costs before you call it rare. Where the frame ends a request's life, a
decode refusal is not a lost message but a permanent stall: the request stays outstanding and
every later one on that connection is refused until the process dies.

## Evidence

- `packages/daemon/internal/shimwire/frames.go` declared `PromptResult.Error` as a `string`. The
  pinned Oh My Pi sends it as an object, `PromptError {message, provider?, model?, httpStatus?,
  retryable}`, on every `prompt_result` whose status is `error`. `shimwire.Decode` refused the
  frame, `stream/conn.go` dropped it, the delivery never retired, and every later delivery on that
  claim was refused `ErrDeliveryPending` after any turn whose provider failed.
- `frames_test.go:69` and `stream/conn_test.go` pinned `"error":"no model"` — a shape the producer
  never writes. Both were green.
- The measurement that settled it: `/opt/omp/bin/omp --mode rpc` against a stand-in Anthropic
  gateway answering `400` to every request, prompted once, wrote
  `{"type":"prompt_result","id":"d1.XYZ","agentInvoked":true,"status":"error","sessionSettled":true,"error":{"message":"400 …","provider":"standin","model":"standin-model","retryable":false,"httpStatus":400}}`.
  A `429` stand-in instead sends that binary into ten automatic retries 30 s apart, and no result
  arrives for twelve minutes.
- That exact line decodes at the fixed head; the same line with `error` as a string gives
  `cannot unmarshal string into Go struct field PromptResult.error of type shimwire.PromptError`,
  which is the frame that used to be dropped.
- `frames_test.go`'s `TestAPromptResultDecodesTheBinarysOwnLineWhole` keeps the producer's lines in
  the suite.
