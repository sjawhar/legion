---
name: legion-retro
description: "Use when the Legion daemon starts you, the implementer, on `Phase: retro` after the architect accepted the reviewed head."
---

# Legion retro

If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong.

Retro runs on the reviewed head, after the architect's acceptance and before the merger merges it
(`skill://legion-worker/references/merge-gate.md` has the whole order).
`<issue>` is your `LEGION_ISSUE`.
Run steps 1-6 even when the change looks mechanical; each gives its reason.

Retro commits nothing and pushes nothing: every handoff is a record on the Dispatch issue, no
commit carries one, and what the work taught is a `dispatch comment` on the issue, for the skill
librarian to read. `docs/solutions/` stays as history, and no role writes there.
The head the merger names is the head the reviewer approved.

## What sends the issue back here

Nothing does.
No Legion check compares the head that merges with the one the review approved, so whether a push
made after the approval must be reviewed again is the repository's own rule, and no handoff is a
commit, so no head carries one for a retro to remove.

Work on the existing issue branch: never rebase it onto another base, never make another branch or
bookmark, and push nothing, because the merger merges this branch's head and a push of yours
would put a commit above the head the reviewer approved.

## When retro stops

When a step fails (the body cannot be computed, read or written, or Dispatch refuses a write), do
not call `handoff_complete`. Report it with `envoy_publish` to the architect's
topic in the `Legion addressing` line of your system prompt, with the command and its output. A
check with no current passing result goes back as step 1 says. The Dispatch message (step 5) is
for a finished retro only.

## Steps

1. **Read the evidence:** the issue, its checks (`dispatch results --issue <issue>`), the pull
   request, and the tester's and reviewer's handoffs
   (`dispatch handoff-read --issue <issue> --phase test`, then the review
   at the version your task's `Handoff:` line names, `--phase review --version <n>`, the command
   that line quotes). Every `before_merge` check of the issue carries a passing result that is
   current at the approved head, the implementer's and the tester's alike, and the pull request
   body's `Checks` block carries each of those lines. In
   cybermonklabs/legion a surface may also be the daemon's real-process tests, a live proof under
   `scripts/e2e/`, or a live check at the operator's next daemon restart recorded on the pull
   request. When a check has no current passing result, or its only evidence is a unit suite,
   call the `legion` tool's `request_backward_move` with `to: implementing` for the implementer's
   result or `to: testing` for the tester's, its `reason` naming the check by its block id. When
   retro runs again, that gap is your first learning.
2. **Get a second reading**, because a reader without your context sees what you stopped seeing.
   First list, in your session, the constraints the issue did not state, choices between two
   working options, approaches you abandoned, and code another issue could reuse, so the second
   answer does not set your list. Then run `task(agent="oracle")` (read-only) with the issue, the
   pull request body and the diff in its brief, asking for reusable learnings, and `wait` for its
   answer, since it runs as a job of its own. You decide which points to keep. Drop a point that
   cites no file, symbol, quoted text or commit of this pull request.
3. **Record the learnings.** Write each kept point you can state as a rule a worker on another
   issue could follow as one `dispatch comment` on the issue, per
   `skill://legion-retro/references/learnings.md`, which says what a point must carry for the
   skill librarian to use it. When none qualifies, say so in that comment's place in step 5's
   message and write no comment.
4. **Update the PR body**, per `skill://legion-retro/references/pr-body-content.md`,
   when the repository derives body lines from changed paths.
5. **Post one Dispatch message on the issue**, the record of the retro that the architect reads.
   No `gh issue` write: Legion issues live on Dispatch. The body is at most 32,000 characters,
   Dispatch's message limit, and ends with your attribution footer:

   ```bash
   dispatch message --issue '<KEY>' --body-file - <<'EOF'
   ## Retro Complete

   **Learnings:** the comment of step 3 (`dispatch://<KEY>/comment/<id>`), or none: <why no point qualified>.

   **Key takeaways:**
   - <one to three reusable lessons>

   **Results read:** implementer <check and result>, tester <check and result>.

   **Production check:** <the command or user action that will show the change working in production>
   **Carried by:** <the deploy or restart that carries the merge there>
   **Run by:** <the agent or role your system prompt's deployment instructions name; when none, the person to ask>

   <!-- legion: {"session":"<session-id>","phase":"retro"} -->
   EOF
   ```

   The merge's close hands you the production check's first look in this session; the daemon
   wakes a later look at a deploy only after you report `pending`
   (`skill://legion-worker/references/merge-gate.md#the-production-check-after-the-close`).
6. **Complete** with the `legion` tool's `handoff_complete`, its summary two sentences for the
   architect. It reports the commit your workspace stands on, which is the head you found: retro
   writes no handoff and makes no commit. In the workspace retro changes nothing; outside it, it
   writes only step 4's `mktemp -d` directory, the PR body and the Dispatch comment and message.
   It sets no GitHub or Dispatch label: Legion keeps phase state in the daemon, not in labels.

## Before you complete

- `jj -R "$LEGION_WORKSPACE" diff --from "legion/${LEGION_ISSUE:?}@origin" --to @ --name-only`
  lists nothing: retro leaves the workspace as it found it, and the merger merges the head the
  reviewer approved.
- Each body line you added or changed that says work was done names work this pull request did.
- The Dispatch comment and the Dispatch message are posted, and the pull request's head is the
  head you found.
