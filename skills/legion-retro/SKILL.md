---
name: legion-retro
description: "Use when the Legion daemon starts you, the implementer, on `Phase: retro` after the architect accepted the reviewed head."
---

# Legion retro

If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong.

Retro runs on the reviewed head, after the architect's acceptance and before the merger's READY
(its `handoff_complete` with `ready: true`, which the daemon checks;
`skill://legion-worker/references/merge-gate.md` has the whole order). `<issue>` is your
`LEGION_ISSUE`. Run steps 1-7 even when the change looks mechanical; each gives its reason.

Retro commits the text fixes that close the pull request's open review threads, and nothing else.
Every handoff is a record on the Dispatch issue, no commit carries one, and what the work taught
is a `dispatch comment` on the issue, for the skill librarian to read. `docs/solutions/` stays as
history, and no role writes there.
The head the merger names is the head your own commit made, or the head you found when the
approval left no thread open.

## What READY refuses

- **A change after the approved head** (`READY_UNREVIEWED_CHANGE`): the
  daemon diffs the reviewer's approved head against the head on GitHub, and the issue goes back to
  the reviewer for another review. READY lets through your own commit of the text fixes that close
  the review threads (step 5), a commit that changes only `docs/solutions/`, which no role writes
  any more, and a file under `.legion/` that GitHub lists as removed: the commit
  `legion handoffs import` makes, never one of yours. Above the approved head an ordinary head
  carries step 5's commit and nothing else of yours.
- `READY_UNREVIEWED_CHANGE` has more causes than a path outside retro's own. Every one of them
  sends the head back to review and none is an error to retry; the complete list, with each
  remedy, is the merger's own role prompt part.

Work on the existing issue branch: never rebase it onto another base, never make another branch or
bookmark, and push nothing but step 5's one commit of the text fixes, because READY reads this
branch's head and any other push of yours would void the reviewer's approval.

## When retro stops

When a step fails (the body cannot be computed, read or written, Dispatch refuses a write, or
GitHub refuses a thread reply or a resolution), do
not call `handoff_complete`. Report it with `envoy_publish` to the architect's
topic in the `Legion addressing` line of your system prompt, with the command and its output. A
check with no current passing result goes back as step 1 says, and a red at your own head as step
5 says. The Dispatch message (step 6) is for a finished retro only.

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
   skill librarian to use it. When none qualifies, say so in that comment's place in step 6's
   message and write no comment.
4. **Update the PR body**, per `skill://legion-retro/references/pr-body-content.md`,
   when the repository derives body lines from changed paths; recompute them at the head step 5's
   commit made when that commit changes a path those lines derive from.
5. **Close the pull request's open review threads.** The approval left each text finding as the
   inline thread the reviewer wrote, and no round follows an approval, so nobody will name those
   threads to you.
   1. List the pull request's unresolved threads yourself, with the `reviewThreads` GraphQL query
      of `skill://legion-worker/references/review-threads.md`.
   2. Make the text fixes in one commit of your own and nothing else, as the workspace rules of
      your role prompt have you make any commit.
   3. Reply on each thread, one disposition reply per thread: `Fixed in <commit>: <one line>` for
      a fix, and the `Declined: <reason>` form the same reference defines for a thread that is
      not a defect — a bot thread the reviewer accepted, one the reviewer declined, and one you
      judge sound.
   4. Resolve each thread you answered with `resolveReviewThread`, one call per thread, never in
      bulk, never a thread you have not read.
   5. Bring the pull request's `Threads:` count and its per-thread entries to the head your commit
      made, each entry stamped with that head (`skill://legion-worker/references/pr-body.md` holds
      the section's shape).
   6. Push the commit to the issue branch, then wait for that head's required checks, the base
      branch's own required set. This is the one place retro waits for CI: subscribe to
      `github.<owner>.<repo>.pr.<n>.>` and end the turn (`skill://envoy`, *Wait for CI or a
      merge*), never `gh run watch`, never `gh pr checks --watch`, never a `sleep` loop.
   7. A red your own fix caused is yours to fix where you stand. Any other red, and a thread you
      cannot close because the sentence is false only where the code is wrong, is the `legion`
      tool's `request_backward_move` to `implementing`, its reason naming what you found.

6. **Post one Dispatch message on the issue**, the record of the retro that the architect reads.
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
7. **Complete** with the `legion` tool's `handoff_complete`, its summary two sentences for the
   architect. It reports the commit your workspace stands on, which is the head step 5's commit
   made, or the head you found when the approval left no thread open: retro writes no handoff.
   Besides that commit the workspace is as you found it; outside it, retro writes step 4's
   `mktemp -d` directory, the PR body, the thread replies and resolutions, and the Dispatch
   comment and message.
   It sets no GitHub or Dispatch label: Legion keeps phase state in the daemon, not in labels.

## Before you complete

- `jj -R "$LEGION_WORKSPACE" diff --from "legion/${LEGION_ISSUE:?}@origin" --to @ --name-only`
  lists nothing: the text fixes are in the commit you pushed, and the workspace holds nothing
  besides. READY checks the approved head the review round recorded, which `read_record` does not
  show.
- Each body line you added or changed that says work was done names work this pull request did.
- The Dispatch comment and the Dispatch message are posted, every thread you answered is
  resolved, and the pull request's head is the head your commit made, with that head's required
  checks green — or the head you found, when the approval left no thread open.
