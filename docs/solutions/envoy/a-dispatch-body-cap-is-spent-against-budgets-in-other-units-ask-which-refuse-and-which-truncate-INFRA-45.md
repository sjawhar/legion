---
title: "A Dispatch body cap is spent against budgets in other units: ask which refuse and which truncate"
category: envoy
tags:
  - dispatch
  - limits
  - margin
  - embeddings
date: 2026-10-22
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "INFRA-45"
  - "sjawhar/legion#2097"
---

# A Dispatch body cap is spent against budgets in other units: ask which refuse and which truncate

Dispatch counts a comment, comment-edit, message or broadcast body in **UTF-16 units**
(`contracts.DispatchBodyMax`, `len16` in `packages/envoy/internal/dispatch/api/server.go`). Every
other budget the same body is spent against counts something else, so "sixteen times the cap" is
never "sixteen times the consumption": ASCII spends one byte a unit, ordinary non-ASCII text three.

Before moving the cap, enumerate those budgets and classify each by what it does when the body
exceeds it.

| Budget | Unit | Where | On exceeding |
| --- | --- | --- | --- |
| Document margin, per thread record (256 KiB) and per document (1 MiB) | bytes | `dispatch/docs/growth.go`, `pmdoc/elements.go` | refuses `413 CAP_EXCEEDED` |
| Embedding input (`maxInputChars`, 32,000) | runes | `dispatch/embed/embed.go` | **truncates silently** (`text.HeadRunes`) |
| Dashboard Agent-view turn (`AGENT_STREAM_LIMITS.partChars`, 16,000) | characters | `packages/pi-envoy/src/agent-stream.ts` | cuts, with "… truncated" |
| HTTP request (1 MiB) and the NATS publish | bytes | `dispatch/api/server.go`, NATS default | refuses / drops the delivery |

- **A budget that refuses owes a documented sentence and a test of the path**, even when the
  architect accepts the consequence with no code change. "Accepted with no work" means no code
  change, not no doc and no test: a caller who now meets a `413` where they did not before learns
  it from the refusal or from nothing.
- **A budget that truncates silently owes a comment linking the two constants**, because nothing
  else will fail when the cap passes it.
- Write the consequence where a caller meets it, and quote the server's own refusal wording rather
  than a sharper sentence of your own: the caller reads the refusal, so documentation that says
  something else is worse than documentation that is slightly imprecise.

## Hazard this change left adjacent, not crossed

`embed.maxInputChars` is 32,000 **runes** and `DISPATCH_BODY_MAX` is now 32,000 **UTF-16 units**.
A body's UTF-16 length is never less than its rune count, so a body at the cap is at most 32,000
runes and nothing truncates today. The two numbers are now equal, with nothing linking them: the
next raise of the body cap starts truncating comment embeddings silently, degrading meaning search
for the tail of a long comment with no error and no failing test. Anyone moving the body cap again
reads `embed.go` first.

## Evidence (INFRA-45, sjawhar/legion#2097)

The cap moved 2,000 → 32,000 UTF-16 units. The margin consequence was the one a reader meets: an
anchored comment is projected into its document's margin whole, so at the new cap a thread record
holds roughly eight full-length ASCII comments where it held about 131, and a document's margin
refuses every further comment and suggestion `413 CAP_EXCEEDED` until the document is split. The
bounds themselves did not move. It is written in
`docs/site/src/content/docs/dispatch/for-agents.md` beside the body limit and in the change's
`packages/envoy/CHANGELOG.md` entry, and
`packages/envoy/internal/dispatch/api/body_cap_margin_test.go` holds the path: a comment anchored
to a document at exactly the cap is accepted and the margin record read back carries the body
whole, which is what makes the `413` reachable rather than theoretical.

The embedding spend (up to sixteenfold per comment, about 8,000 estimated tokens against
`embedqueue`'s 200,000-per-minute budget) and the Agent-view cut were both raised in planning and
accepted with no code change; neither crosses a boundary at this cap. The `maxInputChars`
adjacency above was found in the retrospective, after review.
