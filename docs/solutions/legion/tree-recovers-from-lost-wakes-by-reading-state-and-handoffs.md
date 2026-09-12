---
title: "When the daemon's wakes are lost, the tree still advances: the architect reads daemon state and committed .legion handoffs directly, and every role message names the head it is about"
category: legion
tags:
  - legion
  - architect
  - worker
  - wake-loss
  - phase-complete
  - handoff
  - envoy
date: 2026-09-13
status: active
module: daemon, skills/legion-architect, skills/legion-worker
related_issues:
  - "LEGION-11"
  - "sjawhar/legion#955"
  - "LEGION-29"
  - "LEGION-37"
  - "sjawhar/legion#970"
symptoms:
  - "legion handoff complete: [handoff] Warning: phase recorded; no architect was live to receive the summary"
  - "legion handoff complete: Unable to report phase completion (409): Phase for <KEY> is no longer owned by this worker"
  - "An architect waits for a phase-complete that never arrives while the worker's handoff is already committed and pushed"
---

# The Tree Recovers From Lost Wakes by Reading State and Handoffs

## Context

LEGION-11 ran seven implementer rounds (implement, three rebases, a review fix, the `.legion/`
deletion, retro), three tester rounds, and two review rounds over 23 hours. During that time the
daemon's wake path failed in the two known ways: the Envoy listener recreated its subscriptions
and dropped the architect's role claim (LEGION-29 / #970), so several `legion handoff complete`
calls landed on the 202 no-holder path; and one completion was expected to answer 409 "no longer
owned by this worker" (LEGION-37). Not one `phase-complete` event was needed for the tree to
reach approval. This note records the recovery pattern that made that true, so the next tree
under the same conditions does not stall. The mechanics of 202 vs 409 are in
`worker-pane-shell-gotchas.md` §7 and §11 and `phase-complete-stranded-on-no-holder.md`; this is
the operating pattern on top of them.

## The pattern

**The committed handoff is the message; the wake is only a doorbell.** Every phase writes its
result to `.legion/<phase>.json`, commits it on the issue branch, and pushes before it calls
`legion handoff complete`. The architect therefore never needs the event: it reads the branch.
On LEGION-11 the architect's next assignment each time cited the handoff fields directly
("the tester passed every acceptance line at head 347c7bcd (see `.legion/test.json`,
`round4`)"), which also told the worker exactly which head the instruction was about.

**Each assignment names the head and the artifact it responds to.** A worker revived after a
gap re-reads the issue and `.legion/` on start (the skill requires it); an assignment that says
"rebase — GitHub reports CONFLICTING at head 7dccf398 against main 4d516a1f in `processes.ts`
and `index.test.ts`" is verifiable from the workspace in one `jj log` and one `pr view`. An
assignment that says "fix the conflicts" is not.

**Records accumulate; they never overwrite.** `legion handoff write` replaces the file, so a
worker reporting a later round loads the current file, adds a keyed object (`rebase2`,
`rebase3`, `rebase4`, `review1`), strips the ledger's reserved fields (`phase`, `completed`,
`schemaVersion` — the CLI rejects them), and writes the whole thing back. The reviewer then
found the rebase rationale, the diff-identity result, and each nit's disposition without
asking anyone. The tester did the same: round 1 at the top level, then `round2`…`round4`.

**A 202 needs no action from the worker; a 409 needs one message.** After
`no architect was live`, the daemon holds the completion and replays it on the architect's next
`/process/ready`; the worker stops. After a 409 the daemon holds nothing: the worker publishes
the same one-line summary to `notifications.role.<architect token>` with `envoy_publish` and
stops — never retries, never rewrites the handoff. On LEGION-11 the architect pre-empted this by
putting "your `legion handoff complete` may 409 (LEGION-37) — use `envoy_publish` either way" in
the assignment, which removed the decision from the worker entirely.

**The reviewer's local commits ride on the implementer's next push.** The review App has no
`contents` permission, so `.legion/review.json` sits unpushed on the shared workspace's chain.
The implementer builds on it (confirm with `jj log -r '<sha> & ancestors(@-)'` before pushing)
and the deletion commit removes it with the rest. Rebasing the whole chain
(`jj rebase -s 'roots(main@origin..@)' -d main@origin`) keeps those commits in order.

## Rules

1. Read `.legion/<phase>.json` on the branch before asking a role what it did; the file is the
   durable copy and outlives every wake.
2. Every assignment and every completion summary names the head SHA it refers to.
3. Append keyed round objects to your phase's handoff; never overwrite a prior round.
4. 202: stop. 409: one `envoy_publish` to the architect's role topic, then stop.
5. Build on and carry other roles' unpushed commits; never rebase only your own.

## Related

- `phase-complete-stranded-on-no-holder.md` — what the daemon does with a 202 and what a
  manual `envoy_role_set` does not recover.
- `worker-pane-shell-gotchas.md` §7, §11 — the worker-side view of the 202 and the 409.
- `../envoy/heartbeat-role-reassertion-and-regain-hooks.md` — the session-side fix (#970) that
  re-asserts a dropped claim.
- `conflict-only-rebases-keep-the-diff-auditable.md` — why the rebase rounds were assignments
  the tree could verify from the branch alone.
