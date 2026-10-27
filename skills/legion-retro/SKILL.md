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

Retro removes nothing: every handoff is a record on the Dispatch issue, and no commit carries one.
Retro pushes only when it commits learnings under `docs/solutions/`, a code push like any other,
which gets its own checks; a retro with no learnings pushes nothing.

## What READY refuses

- **A change after the approved head outside `docs/solutions/`** (`READY_UNREVIEWED_CHANGE`): the
  daemon diffs the reviewer's approved head against the head on GitHub, and the issue goes back to
  the reviewer for another review. Commits that change only `docs/solutions/` leave the approval
  standing. READY also lets through a file under `.legion/` that GitHub lists as removed: the
  commit `legion handoffs import` makes, never one of yours.

Work on the existing issue branch: never rebase it onto another base, never make another branch or
bookmark, and push only fast-forward, because READY reads this branch's head and a forced push can
drop another role's commits.

## When retro stops

When a step fails (the body cannot be computed, read or written, or the push is refused), push
nothing more and do not call `handoff_complete`. Report it with `envoy_publish` to the architect's
topic in the `Legion addressing` line of your system prompt, with the command and its output. A
missing proof goes back as step 1 says. The Dispatch message (step 6) is for a finished retro only.

## Steps

1. **Read the evidence:** the issue and its acceptance criteria, the pull request, and the tester's
   and reviewer's handoffs (`dispatch handoff-read --issue <issue> --phase test`, then
   `--phase review`). Confirm the body has an `E2E (implementer)` and an `E2E (tester)` line, each
   a proof as `skill://legion-worker/references/pr-body.md#what-a-proof-is` defines it. In
   sjawhar/legion a surface may also be the daemon's real-process tests, a live proof under
   `scripts/e2e/`, or a live check at the operator's next daemon restart recorded on the pull
   request. When either line is missing or links only a unit suite, commit nothing and call the
   `legion` tool's `request_backward_move` with `to: implementing` for your own proof or
   `to: testing` for the tester's, its `reason` naming the missing line. When retro runs again,
   that gap is your first learning.
2. **Get a second reading**, because a reader without your context sees what you stopped seeing.
   First list, in your session, the constraints the issue did not state, choices between two
   working options, approaches you abandoned, and code another issue could reuse, so the second
   answer does not set your list. Then run `task(agent="oracle")` (read-only) with the issue, the
   pull request body and the diff in its brief, asking for reusable learnings. You decide which
   points to keep. Drop one that cites no file, line or commit of this pull request.
3. **Commit the learnings.** Start your turn's first commit with the `jj log -r @ -T 'empty'`
   check your role prompt gives, so it does not carry the previous role's author. Write each kept
   point you can state as a rule a worker on another issue could follow as a document under
   `docs/solutions/`, per `skill://legion-retro/references/learnings.md`. When none qualifies,
   commit none, and step 5 pushes nothing.
4. **Update the PR body before the push**, per `skill://legion-retro/references/pr-body-content.md`,
   when the repository derives body lines from changed paths.
5. **Push all your commits in one push**, so one CI run covers them, when step 3 committed any:

   ```bash
   cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" bookmark set "legion/${LEGION_ISSUE:?}" -r @- && jj -R "$LEGION_WORKSPACE" git push --bookmark "legion/${LEGION_ISSUE:?}"
   ```

   First check that `jj -R "$LEGION_WORKSPACE" log -r @- --no-graph -T description` has none of
   GitHub's bracket keywords (`[skip ci]`, `[ci skip]`, `[no ci]`, `[skip actions]`,
   `[actions skip]`), which start no workflow: a base that requires a check will not merge a head
   whose checks never ran.
6. **Post one Dispatch message on the issue**, the record of the retro that the architect reads.
   No `gh issue` write: Legion issues live on Dispatch. The body is at most 32,000 characters,
   Dispatch's message limit, and ends with your attribution footer:

   ```bash
   dispatch message --issue "$LEGION_ISSUE" --body-file - <<'EOF'
   ## Retro Complete

   **Learnings documented in:** docs/solutions/<path>.md, or none: <why no point qualified>.

   **Key takeaways:**
   - <one to three reusable lessons>

   **Proofs read:** implementer <surface/command>, tester <surface/command>.

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
   architect. It reports your workspace's `@-`, so push first when you committed learnings. Retro
   writes no handoff. In the workspace retro changes only `docs/solutions/`; outside it, it writes
   only step 4's `mktemp -d` directory and the PR body. It sets no GitHub or Dispatch label: Legion
   keeps phase state in the daemon, not in labels.

## Before you complete

- Before the push, `jj -R "$LEGION_WORKSPACE" diff --from "legion/${LEGION_ISSUE:?}@origin" --to @- --name-only`
  lists only `docs/solutions/` paths, or nothing when you committed no learning. It covers your
  commits; READY checks the approved head the review round recorded, which `read_record` does not
  show.
- Each body line you added or changed that says work was done names work your commits did.
- `gh pr view <n> -R <owner>/<repo> --json headRefOid -q .headRefOid` is your `@-`, and the
  Dispatch message is posted.
