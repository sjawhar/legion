---
title: "A conflict that arrives while another role holds the phase is prepared in the working copy, reported, and pushed only after the backward move"
category: legion
tags:
  - conflict-resolution
  - phase-order
  - backward-move
  - implementer
  - legion-state
date: 2026-10-11
status: active
module: skills/legion-worker
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---

# A conflict that arrives while another role holds the phase is prepared in the working copy, reported, and pushed only after the backward move

- GitHub can start reporting a pull request `CONFLICTING` at any phase: a sibling merges into
  `main` while the tester or reviewer holds the issue. The conflict is the implementer's to
  resolve, but the workspace and the push are the active phase's: a push under the tester moves
  its head mid-round, and the daemon counts the implementer's completion for a phase it does not
  hold as `HANDOFF_NOT_CURRENT_PHASE`.
- So split the work at the push. Fetch, forward-merge `main` (`jj new legion/<KEY> main@origin`),
  resolve, renumber, run the checks and the scratch proof, compute both fingerprints — all of it
  in the working copy, which is yours to shape and nobody else reads until it is pushed. Then
  report to the architect's role topic: the prepared commit's SHA and parents, the fingerprints,
  what the checks showed, and that the issue is in `<phase>`. The role holding the phase calls
  `request_backward_move` to `implementing` (an architect cannot), and the daemon's task for the
  round arrives in your own session.
- Wait on the record, not on the clock: `legion state --json | jq -r '.issues["<KEY>"].phase'`
  every ten seconds until it reads `implementing` (about ninety seconds here, after the architect
  relayed the request to the tester). Fetch once more (`main` may have moved again), then
  `legion push`, the PR body, the handoff and `handoff_complete` as any round.
- Prepare the round's handoff payload while you wait; it is the previous one plus this round's
  commit, files, checks and proof entry, and `handoff_write` recreates `.legion/<KEY>/` after a
  retro removed it.

## Evidence

LEGION-663 (sjawhar/legion#1874). #1878 (LEGION-668) merged declaring daemon API contract 20 at
07:06Z while the issue was in `testing`; the architect wrote "whatever phase the issue is in …
the work is yours … if the daemon has not moved the issue back by the time you are ready to push,
say so on my topic and the role holding the phase moves it". The implementer prepared `6da3ade3`
(forward merge, contract 21, three conflicts composed, both fingerprints, six-row scratch proof),
reported at 07:13Z with `read_record` showing `phase: testing`, and polled `legion state` from
07:15:41 (`testing`) to 07:16:42 (`implementing`) after the tester's `request_backward_move`;
the push, body, handoff and completion followed in the same turn and the pull request read
`MERGEABLE` again.
