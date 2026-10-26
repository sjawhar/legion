---
title: "A wire field whose empty object means something gets a presence guard, not the block's conditional-spread idiom"
category: best-practices
tags:
  - wire-contracts
  - dispatch-tools
  - request-building
  - destructive-defaults
date: 2026-10-25
status: active
module: packages/envoy-client/src/dispatch-execute.ts, packages/envoy/internal/dispatch/api/issue_rank.go
related_issues:
  - "INFRA-188"
  - "sjawhar/legion#2174"
---

# A wire field whose empty object means something gets a presence guard, not the block's conditional-spread idiom

Before adding a **nested object** argument to a request a block already builds field by field, ask
the server what an empty object of that field means. Where its presence alone triggers work, the
house idiom is wrong by construction.

- The idiom every scalar field uses, `...(x === undefined ? {} : { x })`, composes into
  `field: {}` when a nested object's own members are all absent — the one value that must never
  be sent. Build the whole object first, in a named local, `undefined` when no member is present,
  and spread the result: `...(field === undefined ? {} : { field })`. The guard is then one
  expression, read by both the request and anything that prints the change.
- Put the reason in a comment on that local, not in the commit message. The next editor meets the
  local, not the history.
- Decide per argument whether an empty string is a clear or a value, and say which in the
  argument's own description. This command's convention is "empty clears" (`--parent ''`,
  `--route ''`, `--repo ''`); an argument that breaks it must reach the server, so the server's
  refusal is what the caller sees instead of a silent success.
- Pin both halves in one test: the key absent for a call that names no member, and the key
  present for the empty-string member. A falsy or trimming guard added later passes the first and
  fails the second, which is exactly the regression — a success message over a write that did
  nothing.
- A line the command prints from its own arguments proves nothing about the server. Where the
  change is a reordering or any other server-computed position, the only instrument that can see
  a semantics change is re-reading the resource after the call; keep that re-read in the proof
  for every later change to that path.

## Evidence (INFRA-188, sjawhar/legion#2174)

`dispatch issue-update` gained `--before <KEY>` / `--after <KEY>`, which travel as the Dispatch
server's existing `rank` field on the PATCH the command already sends.

`rank` present with neither neighbour is not a no-op: `rankForInput`
(`packages/envoy/internal/dispatch/api/issue_rank.go:46-53`) falls through to `lastProjectRank`
and the issue is ranked **last in its project** — that is how the web board's move-to-the-end
works. The executor's eleven other fields are each spread as
`...(x === undefined ? {} : { x })` (`packages/envoy-client/src/dispatch-execute.ts:2895-2904`),
and a nested object of two `undefined`s serialises to `"rank":{}`. Written that way, every
ordinary `dispatch issue-update --status testing` would silently have dropped its issue to the
bottom of its board.

The hazard was found in planning, not in review: the plan's gap analysis named it before any code
was written, and the shipped code computes the object first
(`dispatch-execute.ts:2883-2891`) and spreads the result (`:2905`), with the reason in a comment
on the local. The unit lock is the recorded-fetch test
`dispatch_issue_update sends a board move only when a neighbour is named`
(`packages/envoy-client/src/__tests__/dispatch-execute.test.ts:2853-2903`): a status-only call
sends no `rank` key, and `{ before: "" }` sends `rank: { before: "" }`, which the server refuses
with `RANK_INPUT: rank neighbor must name another issue` (`issue_rank.go:102-118`, after the
handler trims the key at `issue_patch.go:167-171`, so `--before '   '` is refused the same way).
The empty-string case was a review finding, not foresight: without it, a later falsy guard would
turn that refusal into a silent no-move.

The live proof is a board re-read, not the printed line. The command composes
`rank -> after <KEY>, before <KEY>` from the arguments it was given
(`dispatch-execute.ts:2945-2952`), never from the server's answer, so every run of the proof
listed the project's board after each call. One observation only that re-read could give: with a
non-adjacent ordered pair the card lands directly below the `--after` neighbour
(`nextProjectRank`, `issue_rank.go:57-70`) — between the two named issues, which is what the
texts promise, but not midway between them.
