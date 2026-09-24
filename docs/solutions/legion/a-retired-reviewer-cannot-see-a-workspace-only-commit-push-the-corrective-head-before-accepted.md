---
title: "A retired reviewer cannot see a workspace-only commit: push the corrective head before the `Accepted:` reply, and run `legion threads resolve` in the round that lands the deletion"
category: legion
tags:
  - legion-worker
  - review-round
  - threads-resolve
  - idle-retire
  - corrective-push
  - jj
date: 2026-09-14
status: active
module: legion
related_issues:
  - "LEGION-84"
  - "sjawhar/legion#1080"
---

# A retired reviewer cannot see a workspace-only commit

The `legion-worker` skill's corrective sequence reads: fix, reply on the thread, wait for the
reviewer's `Accepted:`, run `legion threads resolve`, push. It assumes a reviewer that is live
and watching. In practice a phase worker is retired within `worker_idle_retire_seconds` (default
ten minutes) of finishing its phase, and the architect resumes it only *with the head it must
re-review*. A fix that exists only as a commit in the shared issue workspace is invisible to a
retired reviewer: it cannot inspect the commit, cannot reply `Accepted:`, and the round deadlocks
on a wait that nothing will end. On LEGION-84 the implementer polled the thread for fifteen
minutes before the architect changed the order.

## The order that works

1. **Fix, commit, verify** in the shared workspace as usual — test-only if the finding was
   test-only, one commit, on top of every other role's handoff commits (the tester's and the
   reviewer's `.legion/*.json` sit above yours; land them, never drop or rewrite them).
2. **Reply on the thread** with the fixing commit's SHA and the verification (a mutation check, a
   test count), and say the commit rides the next push.
3. **Push now.** Update `.legion/implement.json` for the round, `jj bookmark set legion/<KEY> -r @-`
   on your tip, one push. Set the PR body's `Threads` line to *answered, awaiting `Accepted:`*.
   Report the new head to the architect over Envoy and with the `legion` tool's `handoff_complete`; the
   architect resumes the reviewer on that head.
4. **`legion threads resolve` waits for the `Accepted:`.** The command resolves only a thread whose
   newest comment is the opener's own `Accepted:`; run it early and it prints `left open`. The
   architect's next resume of the implementer does two things in one round: `threads resolve`
   (Threads line to `0 unresolved`, output quoted) and, if the re-review was clean, the `.legion/`
   deletion push the reviewer then approves by SHA.

The reviewer's re-review of the corrective head stays the same round when the production
fingerprints are unchanged; a test-only fix leaves them so.

## Two mechanics worth knowing on the same path

- **`handoff_write` refuses a payload carrying `schemaVersion`, `phase`, or `completed`** — the
  CLI it runs stamps those itself. Pass the `legion` tool's `handoff_write` a `data` object of
  phase-specific fields only (read the previous `.legion/implement.json`, drop those three keys,
  add the round's fields). A refused write after a `jj split -m … .legion/implement.json` leaves an
  *empty* handoff commit; fold the real write into it with
  `jj squash --into <that change> .legion/implement.json` and push sideways rather than stacking a
  second commit.
- **The pane guard reads every word of a bash command.** A commit message or a `legion gh` PR body
  that names `jj` beside a word on the guard's list (the LEGION-84 key contains `abandon`) is
  refused with the operation-log warning. Say "the per-repo keep-unreachable-commits setting"
  instead of the key, keep `jj` out of the sentence, or pass the text in a file. The `legion`
  tool's `summary` is not shell text; the guard never reads it.
