---
title: "A real-binary refusal proof seeds past every earlier branch, and the proof is the pair"
category: testing
tags:
  - e2e
  - real-binary
  - authorization
  - negative-control
  - dispatch
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "INFRA-797"
  - "sjawhar/legion#2553"
---

# A real-binary refusal proof seeds past every earlier branch, and the proof is the pair

A refusal chain renders one observable. `401 NO_IDENTITY` is what an absent cookie, an unsigned
cookie, a cookie whose person has no session row and a cookie the new guard refuses all look
like from outside the process. A proof that drives the real binary and reads a `401` has
therefore proved nothing until two things hold.

**Seed past every branch that fires before the one under test.** Read the chain, and give the
running process the state each earlier branch needs to pass. For a Dispatch cookie that means
the rows a sign-in leaves behind — `insert into people (email)` and
`insert into user_sessions (login)`, so `CurrentSessionGeneration` finds the login — because
without them `CookieIdentity.Login` refuses at `!found` and the run reads the same `401` whatever
the guard does.

**Record the pair, not the refusal.** Run the same request against the same binary built one
condition different — the guard disabled, the check forced to pass — and quote both answers. The
`401` is evidence only beside the `200` the unguarded build gives. Build that second binary from
the same head, with the change in the working copy, and restore the file afterwards.

Both halves go in the handoff's `proof` entry: the seeded state in `command`, the pair across
`observed` and `negativeControl`.

## Evidence

INFRA-797's authz fix refuses every `dsession` cookie on a cookie server with no sign-in
setting. The first real-binary run was made against a fresh database and answered
`401 NO_IDENTITY` — for the wrong reason: no `user_sessions` row existed, so the pre-fix binary
refused it too, and the run distinguished nothing. With the rows seeded
(`select login, generation from user_sessions` → `someone@example.com|0`), the pair separates:
the head's binary answers `401 {"code":"NO_IDENTITY"}` on `/api/v1/whoami`, `/api/v1/inbox` and
`POST /api/v1/me/agent-tokens`, and the same binary with `if false && …` in front of the guard
answers `200 {"kind":"user","login":"someone@example.com"}`, `200` and `201` with a usable token.
The agent-token API answers `200` on both, which is what shows the guard is scoped to browser
callers.
