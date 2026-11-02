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

**Superseded 2026-09-25 (LEGION-285):** every role now pushes its own commits (`skills/legion-worker/SKILL.md`; the Apps' permissions are in `packages/daemon/src/daemon/AGENTS.md`, GitHub Apps). What follows records the earlier workflow. The order below still holds: the implementer pushes the corrective head before the `Accepted:`, and runs `legion threads resolve` after it.

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
   test-only, one commit, on top of every commit the branch already carries. Before
   sjawhar/legion#2354 the tester's and the reviewer's `.legion/*.json` commits sat above yours,
   and you landed them rather than dropping or rewriting them. A handoff is a record on the
   Dispatch issue now, so only code commits are there.
2. **Reply on the thread** with the fixing commit's SHA and the verification (a mutation check, a
   test count), and say the commit rides the next push.
3. **Push now.** `jj bookmark set legion/<KEY> -r @-` on your tip, one push. Write the round's
   implement handoff with `dispatch handoff-write --issue <KEY> --phase <word> --body-file
   <file>`. Before sjawhar/legion#2354 you updated `.legion/implement.json` for the round and
   pushed it with the fix. Set the PR body's `Threads` line to *answered, awaiting `Accepted:`*.
   Report the new head to the architect over Envoy and with `handoff_complete`; the
   architect resumes the reviewer on that head.
4. **`legion threads resolve` waits for the `Accepted:`.** The command resolves a reviewer's thread only when
   its newest comment is the opener's own `Accepted:` (a bot's thread, since LEGION-208, on the
   Legion reviewer's `Accepted:` too, never the author's reply); run it early and it prints
   `left open`. The
   architect's next resume of the implementer does two things in one round: `threads resolve`
   (Threads line to `0 unresolved`, output quoted) and, if the re-review was clean, the reviewer's
   approval of the head by SHA. Before sjawhar/legion#2354 that approval went to the `.legion/`
   deletion push the round made after the clean re-review.

The reviewer's re-review of the corrective head stays the same round when the production
fingerprints are unchanged; a test-only fix leaves them so.

## Two mechanics worth knowing on the same path

- **A handoff body is one JSON object, and the phase is an argument.** `dispatch handoff-write
  --issue <KEY> --phase <word> --body-file <file>` refuses a body that is not one JSON object, a
  phase that is not a lowercase slug, and a body over the byte cap. Write the file outside the
  workspace, because jj snapshots every file in the workspace. Before sjawhar/legion#2354 the
  `legion` tool's `handoff_write` refused a payload carrying `schemaVersion`, `phase` or
  `completed`, because the CLI stamped those itself, and a refused write after a
  `jj split -m … .legion/implement.json` left an empty handoff commit to fold the real write into.
  No handoff carries a `completed` field since dispatch://INFRA-336: a handoff's time is when
  Dispatch recorded it (`created_at`).
- **The jj operation-log rule reads every word of a bash command.** A commit message or a
  `legion gh` PR body that names `jj` beside a word on the rule's list (the LEGION-84 key contains
  `abandon`) is refused with the operation-log warning. Say "the per-repo keep-unreachable-commits
  setting" instead of the key, keep `jj` out of the sentence, or pass the text in a file. The
  `legion` tool's `summary` is not shell text; the rule never reads it.
