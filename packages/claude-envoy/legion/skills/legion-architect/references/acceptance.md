# Acceptance, retro, the merge and the close

Part of `skill://legion-architect`, sections 3, 5, 6 and 7. Read the section for the step you are
at.

## Retro's result

Retro output is `docs/solutions/`, a last commit removing the issue's `.legion/<issue>/` from the
head that merges, the PR body content the repository's instructions derive from the pull request's
changed paths at that head, and one `dispatch message` on the issue. Retro writes no handoff and
never rewrites the reviewer-approved head.

When the implementer reports that it cannot compute that body content or do the work a line of it
affirms, that its read of the body failed or came back empty, or that GitHub refused the body, it
has pushed nothing and is still in its phase. Answer it with `envoy_publish` to its role topic,
saying how from the deployment instructions; when they do not say, open a `dispatch ask` naming the
missing instruction.

## Accepting

```text
dispatch comment --issue LEGION-40 --body-file - <<'EOF'
<acceptance: scope, integration evidence, the tester's handoff, both E2E lines, the approved head>
EOF
legion({ op: "sign_off", issue: "LEGION-40" })
```

- The issue's Dispatch status stays `needs_review` in `accepting`; `sign_off` moves it to `retro`.
- Only what is beneath the issue counts for `SIGNOFF_CHILD_IN_WORKFLOW`: a child with nothing
  beneath it is accepted while its own root runs.
- `request_backward_move` names the issue, because your grant serves the tree; the daemon starts
  that phase's role with your reason in its task.
- `handoff_complete` from you in `accepting` is refused `ACCEPTANCE_IS_SIGN_OFF`. CI red at the head
  in `accepting` returns the issue to `implementing` by the daemon's table.
- No `Production:` line is asked here: the production check is the implementer's, after the close.

## The order after your acceptance

The daemon keeps this order from its fixed table; you start none of its steps:

1. The tester's last result is green and the reviewer approves the head.
2. The reviewer's approval of the head by SHA moves the issue to `accepting`; your `sign_off` moves
   it to `retro`. The approved head still carries `.legion/<issue>/`.
3. Retro commits its learnings under `docs/solutions/` on top of the approved head, then, as the
   one final commit, the removal of `.legion/<issue>/`, so no merge carries a handoff onto the
   default branch. Those commits do not void the approval and never return the tree to the tester
   or reviewer.
4. The merger quotes what sits above the approved head
   (`jj diff --from <approved-sha> --to <tip-sha> --summary`) in READY and sends the READY packet
   with its completion. The daemon refuses a head carrying anything above the approved head but
   retro's commits (`READY_UNREVIEWED_CHANGE`) and a head that still carries `.legion/<issue>/`. It
   posts `READY #<n> at <current sha> (approved at <approved sha>) for <KEY> (<pr url>)` on the
   Dispatch issue, publishes it to the project's merge queue role when one is set, and submits the
   merge of that head itself, as the implement App with GitHub's expected-head check. The
   repository's required reviews and checks decide when it lands; Legion neither reads nor writes
   those rules, and a review only a person can give is waited for, never bypassed. A READY the
   design gate refused is submitted by the daemon once that spec version is approved.
5. The merge lands under the repository's rules and closes the issue; a root's close waits until
   the rest of its tree has left the workflow.

A refused or failed READY arrives while the merger holds `merging`, so the merger makes the move;
you tell it which (`envoy_publish` to its role topic). These targets are the merger's, not yours:
from `accepting` you send an issue back only to `implementing` or `testing`.

- The merger reports a failed verification: handle it as the `pr-blocked` row of
  `skill://legion-architect/references/wake-routing.md` says. Tell the merger to move the issue
  back with `request_backward_move`, naming what failed; never bypass.
- `READY_UNREVIEWED_CHANGE`, or a merger task with no approved head (`READY_NOT_APPROVED`): the
  merger moves it back to `reviewing`, the reason naming the code and what it found, so the
  reviewer runs a new round.
- Refused because the head still carries `.legion/<issue>/`: the merger moves it back to `retro`,
  so the implementer's retro removes it.

## A conflicting head

What returns the tree to review is a changed diff: a commit above the approved head that touches
anything outside `docs/solutions/` and `.legion/<issue>/`, or a conflict-resolution merge whose
fingerprint (the unchanged-diff check,
`skill://legion-worker/references/conflicts-and-rewrites.md`) differs from the approved head's.
Retro's commits and a merge forced by a GitHub-reported conflict whose fingerprint is unchanged do
not.

Make a conflict-resolution merge only when GitHub reports `CONFLICTING`
(`gh pr view <n> --json mergeable,mergeStateStatus`). Outside `awaiting_merge` the daemon records
a conflict and moves nothing, so the moment you see it:

- In a phase a worker holds, tell that worker (`envoy_publish` to its role topic) to move the issue
  back to `implementing` with `request_backward_move`.
- In `accepting`, make the move yourself (`request_backward_move` with `issue`).
- In `awaiting_merge`, do nothing: the daemon withdraws the READY and sends the issue back to
  `implementing` itself, telling you with a `checks-red` whose reason reads
  `the head conflicts with <base>`.

The daemon then runs the phases from `implementing`: the implementer merges the bookmark forward
with the destination (`jj new legion/<KEY> <destination>`, the forward-merge procedure in
`skill://legion-worker/references/conflicts-and-rewrites.md`; never a rebase, which rewrites every
descendant of the fork point, including another tree's stacked branch), pushes it with
`jj git push` (a genuine fast-forward and a code push, so no `skip-checks: true` line) and posts the
before and after fingerprints; the tester re-runs the bare gates only; the reviewer approves the
new head by SHA, or continues its round if it had not approved; the daemon carries the issue
through your acceptance and retro to the merger's new READY.

## A review thread GitHub will not resolve

GitHub lets only the pull request author's App resolve a thread, so the implementer resolves every
thread with plain `gh`: the ones it answers and the bot threads the reviewer accepted and named to
it. The reviewer resolves none. If the implementer reports that GitHub refused its
`resolveReviewThread`, open a `dispatch ask` naming the thread's id or URL and GitHub's message, for
a human to resolve it by hand, with options for resolved / could not. The merger does not complete
while that ask is open; READY itself checks no thread state
(`skill://legion-worker/references/merge-gate.md`).

## The close

The merge closes the issue: when GitHub reports the pull request merged while the issue is in
`awaiting_merge` (a merge before that closes it the moment it gets there), the daemon writes `done`.

- A root whose merge lands while another issue of its tree is still in the workflow stays in
  `awaiting_merge`, with its slot and workers kept, until the last of them is done or parked. One
  `pr-merged` notice names what it waits for and none follows as they depart, so read the pull
  request on GitHub.
- A child's close reaches you as `child-closed`. The daemon retires five of its six claims and
  leaves the implementer running, because it still owes the production check. When the issue's
  pod and Sandbox go is in `skill://legion-worker/references/merge-gate.md`, *The production check
  after the close*.
- A root's close writes `done`, frees the tree's admission slot and lingers the tree for
  `linger_hours` with every member suspended. The linger's end, the tree's last step, waits until
  every member's open production check has closed.

Nothing of the production check is yours. Its record is the pull request's `Production:` line,
whether or not the issue is closed; a defect it finds is a `dispatch ask`, never an issue; and
`read_record` shows a closed issue's open check (`check`: the pull request, the merge commit, the
looks, what woke the last one and when the next is due).

## Hand-over at a root's linger

When a person closes a root and it stays `done` through its linger, each child still in the
workflow is handed to Legion as a root of its own: `todo` with the `legion` label, its phase, gate,
phase records and pull request carried into a new generation, and that phase's role started. A
child that never started but still waits on an unfinished issue is handed over too, carrying no
run: it waits in the line until its blockers are done, and its architect starts as a fresh root
and registers its own spec. The child stays its own tree; reopening the old root does not take it
back. Park a child with `park_child` before then when its work should stop with the root.

If you are that child's architect:

- Your catch-up says `handed_over` and `carried`: resume from the phase, and never register a spec
  or ask for approval again.
- It says `handed_over` alone (no gate to carry): your workers already run their phases, so
  decompose nothing, and read the root's phase from the catch-up (for a root `held`, the phase its
  `held` notice names). Before `awaiting_merge`, register your spec with `register_gate`: your
  merger's READY waits for its approval. When that `design-approved` arrives, release and start
  nothing: the workers are already running. A child carried in `accepting` is yours to accept. In
  `awaiting_merge` the READY is through, so register none; a red or conflicting head that returns
  the root to `implementing` makes its next READY read the gate again, so a later `ready-refused`
  is when you register.
