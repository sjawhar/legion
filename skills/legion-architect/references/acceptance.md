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

Sign off an issue with no children once its evidence holds; a parent only after its last round
(below), because before it the approved head does not carry its children's merges.

```text
dispatch comment --issue LEGION-40 --body-file - <<'EOF'
<acceptance: scope, integration evidence, the tester's handoff, both E2E lines, the approved head>
EOF
legion({ op: "sign_off", issue: "LEGION-40" })
```

- The issue's Dispatch status stays `needs_review` in `accepting`; `sign_off` moves it to `retro`.
- A `catch-up` whose `reason` names an issue as waiting for your acceptance is the wake for it: it
  is in `accepting`, and the `phase-finished` notice that moved it there may never have reached you
  (the daemon drops a notice to a failed architect, so a retried one learns of the wait from its
  catch-up alone). Accept it as this section says, a parent only after its last round (below).
- The daemon lets nothing beneath the issue hold `sign_off`. A parent: sign off only after its
  last round (below). The daemon then closes a parent that is not the root at its own merge, which
  under this rule comes after its children's, and those children carry on under the root.
- `request_backward_move` names the issue, because your grant serves the tree; the daemon starts
  that phase's role with your reason in its task.
- `handoff_complete` from you in `accepting` is refused `ACCEPTANCE_IS_SIGN_OFF`. CI red at the head
  in `accepting` returns the issue to `implementing` by the daemon's table.
- No `Production:` line is asked here: the production check is the implementer's, after the close.
- A finding ask on the issue holds the acceptance: one a worker sent you, or one `dispatch read`
  lists under `Open asks:` whose question opens `Finding from ` or `Clear defect from `
  (`skill://legion-worker/references/findings.md`, "When no controller takes it"). Follow each
  (`dispatch follow --ask <id> --action follow`) and, once it is answered, publish its reference
  to the controller topic. Accept only once each is resolved, or carries the controller's
  `Filed dispatch://`, `Handed dispatch://` or `Not filed:` reply, because a closed issue hides its
  asks from the controller.

## The last round

A parent's own pull request proves each of its criteria, so sign off a parent that has children
only on an approved head that carries the merges of every child those criteria need. Each
criterion needs tester evidence at that head, or, for a criterion in another repository, on that
repository's default branch. Nothing in the tree builds on the parent's own merge, because that
merge comes last: a child that waits for it waits forever, so work another child builds on is a
first-wave child of its own. While the approved head does not carry those merges, or the tester's
evidence of a criterion in another repository is older than the close of a child it needs, leave
the issue in `accepting` until each of those children has closed (`child-closed`) or been parked
for good (`child-status`). Move a parked child's criterion to other work first; never downgrade
it. Then,
in one turn:

1. `envoy_publish` to the tester's role topic: "The next round of <KEY> is its last round: a full
   test round of every acceptance criterion, not the forward-merge re-check." The backward move's
   reason reaches only the implementer.
2. `request_backward_move` to `implementing`, with the reason "Last round: merge the default
   branch your addressing line names into the branch, push, end your forward-merge comment with
   `parent's last round: full test round`, and complete." Never use a hard-coded `main`. The move
   counts a review round.

While the parent waits in `accepting`, a conflict on its head or a child's merge needs no move of
its own: the last round's forward merge resolves them all, so spend no backward move per child,
because every backward move counts toward `review_round_cap`.

A criterion proven on a deployed system is proven there after that child's deploy, as the
repository's deploy skill says.

If the pull request merged before its last round, it can carry no new head, so move nothing back.
Once those children have closed, have a fresh `task` subagent prove each criterion on the default
branch, post that evidence in the acceptance comment, then `sign_off`.

## The order after your acceptance

The daemon keeps this order from its fixed table; you start none of its steps:

1. The tester's last result is green and the reviewer approves the head.
2. The reviewer's approval of the head by SHA moves the issue to `accepting`; your `sign_off` moves
   it to `retro`. The approved head still carries `.legion/<issue>/`.
3. Retro commits its learnings under `docs/solutions/` on top of the approved head, then, as the
   one final commit, the removal of `.legion/<issue>/`, so no merge carries a handoff onto the
   default branch. Those commits do not void the approval and never return the tree to the tester
   or reviewer.
4. The merger reads the pull request and merges that head itself, as the implement App and with the
   head it read as the head GitHub must match
   (`gh pr merge <n> --squash --match-head-commit <head> --subject '<pull request title> (#<n>)' --body ''`,
   whose explicit message keeps GitHub's default squash message from joining the branch's commit
   messages onto the base; or `expectedHeadOid` on a
   merge-queue base, where it enqueues the pull request); it never turns auto-merge on, and the
   daemon submits no merge and changes no merge setting. A head that still carries
   `.legion/<issue>/` goes back to retro instead of merging. The repository's required reviews and
   checks decide when it lands; Legion neither reads nor writes those rules, and a review only a
   person can give is waited for, never bypassed. Nothing compares that head with the approved one:
   a repository that wants a later push reviewed says so in its own rulesets. The merger's
   completion records its phase row and moves nothing, and the daemon posts no packet.
5. The merge lands under the repository's rules, the daemon reads it and closes the issue; a
   root's close waits until the rest of its tree has left the workflow.

A merge the merger cannot make arrives while the merger holds `merging`, so the merger makes the
move; you tell it which (`envoy_publish` to its role topic). These targets are the merger's, not
yours: from `accepting` you send an issue back only to `implementing` or `testing`.

- The merger reports a failed verification: handle it as the `pr-blocked` row of
  `skill://legion-architect/references/wake-routing.md` says. Tell the merger to move the issue
  back with `request_backward_move`, naming what failed; never bypass.
- A merge GitHub refuses for a repository setting or an App permission — a merge-queue base whose
  `squash_merge_commit_message` joins the branch's commit messages among them: the merger tells
  you, and only a person can change it. Ask one with `dispatch ask`, naming the pull request, the
  head and GitHub's refusal; never bypass a rule.
- A head that still carries `.legion/<issue>/`: the merger moves it back to `retro`,
  so the implementer's retro removes it.

## A conflicting head

What returns the tree to review is a changed diff: a conflict-resolution merge whose
fingerprint (the unchanged-diff check,
`skill://legion-worker/references/conflicts-and-rewrites.md`) differs from the approved head's.
Retro's commits and a merge forced by a GitHub-reported conflict whose fingerprint is unchanged do
not, and no Legion check compares the head that merges with the approved one: a repository that
wants a later push reviewed says so in its own rulesets.

Make a conflict-resolution merge only when GitHub reports `CONFLICTING`
(`gh pr view <n> --json mergeable,mergeStateStatus`). From `testing`, `reviewing`, `accepting`,
`retro` and `merging` alike the daemon acts on it itself: it moves the issue back to
`implementing`, starts the implementer, and tells you with a `checks-red` notice naming the phase
the issue was taken out of and whose reason reads
`the head conflicts with <base>: GitHub runs no checks on it; merge <base> forward`. You start
nothing for it.

The daemon then runs the phases from `implementing`: the implementer merges the bookmark forward
with the destination (`jj new legion/<KEY> <destination>`, the forward-merge procedure in
`skill://legion-worker/references/conflicts-and-rewrites.md`; never a rebase, which rewrites every
descendant of the fork point, including another tree's stacked branch), pushes it with
`jj git push` (a genuine fast-forward and a code push, so no `skip-checks: true` line) and posts the
before and after fingerprints; the tester re-runs the bare gates only; the reviewer approves the
new head by SHA, or continues its round if it had not approved; the daemon carries the issue
through your acceptance and retro to the merger's merge.

## A review thread GitHub will not resolve

GitHub lets only the pull request author's App resolve a thread, so the implementer resolves every
thread with plain `gh`: the ones it answers and the bot threads the reviewer accepted and named to
it. The reviewer resolves none. If the implementer reports that GitHub refused its
`resolveReviewThread`, open a `dispatch ask` naming the thread's id or URL and GitHub's message, for
a human to resolve it by hand, with options for resolved / could not. The merger does not complete
while that ask is open; the merge itself checks no thread state
(`skill://legion-worker/references/merge-gate.md`).

## The close

The merge closes the issue: the daemon reads the merged pull request — from GitHub's webhook or
from its own read of the issue — and writes `done`. An issue that reaches `merging` on a pull
request GitHub has already merged closes at once.

- A root whose merge lands while another issue of its tree is still in the workflow stays in
  `merging`, with its slot and workers kept, until the last of them is done or parked. One
  `pr-merged` notice names what it waits for and none follows as they depart, so read the pull
  request on GitHub.
- A child's close reaches you as `child-closed`. The daemon retires five of its six claims and
  leaves the implementer running, because it still owes the production check. When the issue's
  pod and Sandbox go is in `skill://legion-worker/references/merge-gate.md`, *The production check
  after the close*.
- A root's close writes `done`, frees the tree's admission slot and lingers the tree for
  `linger_hours` with every member suspended. The linger's end, the tree's last step, waits until
  every member's open production check has closed.

Nothing of the production check is yours. Its record lives on the pull request alone, since
Dispatch refuses a write on a closed issue. A defect it finds is never an issue the implementer
files: a clear defect goes to the controller, and any other is a `dispatch ask`. `read_record`
shows a closed issue's open check (`check`: the pull request, the merge commit, the looks, what
woke the last one and when the next is due).

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
  `held` notice names). Before the merge, register your spec with `register_gate`: the tree's
  work starts on its approval. When that `design-approved` arrives, release and start
  nothing: the workers are already running. A child carried in `accepting` is yours to accept. In
  `merging` the work is already through every gate, so register none.
