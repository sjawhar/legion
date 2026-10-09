# The merge gate: review, retro, READY, and the production check

Part of `skill://legion-worker`. Read it when you are the reviewer submitting a review or an
approval, the implementer recording the production check, or the merger building READY. Every
path it cites is in sjawhar/legion.

The order, in full: the tester's evidence green → the reviewer's approval of the head →
retro → the merger's READY → the human merge → the implementer's production check. The approved
head carries the issue's handoffs, `.legion/<issue>/`; retro's last commit removes them from the
head a human merges (dispatch://LEGION-605), since a squash merge commits that head merged into
the default branch and nothing there reads a handoff, and READY refuses a head that still
carries them. After the approval, only retro's commits leave it standing on their own: those that
change only `docs/solutions/`, and that removal (*Retro*, below). A conflict-forced merge goes back
to the reviewer for a confirmation or a new round, as the fingerprint decides (*The reviewer*,
below, and `skill://legion-worker/references/conflicts-and-rewrites.md`), and any other change to
the head voids it.

## The reviewer

- The reviewer verifies the `CI`, `Threads`, and `E2E` facts against GitHub directly —
  never from a handoff — then runs `task(agent="thermonuclear-deep-review")` and
  `task(agent="thermonuclear-code-quality")` once at that head — the head the round reviews —
  and records the verdict.
  Approval is refused while either `E2E (implementer)` or `E2E (tester)` is missing: `REQUEST_CHANGES` naming the missing line.
  Skip the `Thermo` line entirely on a docs-only PR. Submit **one review per round** —
  `REQUEST_CHANGES` when any correctness finding stands, otherwise `APPROVE` of the head you
  reviewed — always named by SHA — carrying every inline comment in that single
  call: `gh api --method POST repos/{owner}/{repo}/pulls/{number}/reviews --input body.json`
  with `commit_id`, `event` (`REQUEST_CHANGES` or `APPROVE`), `body` (with the
  Legion footer), and a `comments[]` array of `{path, line, side, body}`, one entry per
  finding — never one `pr review` call per finding (each submission fires a `pr-review` wake).
  A `COMMENT` decides nothing. After a
  conflict-forced rebase, compute the fingerprint (*The unchanged-diff check* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`) at the
  `commit_id` of your last submitted review and at the new head. Equal and that review was
  `APPROVE`: submit one more `APPROVE` naming the new head by SHA, its body naming both SHAs
  and the fingerprint — a confirmation, not a round; no thermo pass, no thread pass. Equal and
  that review was `COMMENT` or `REQUEST_CHANGES`: continue that round against the new head;
  nothing restarts. Different: a new round — thermo again, one review.
- Answer every thread you opened, and every thread a bot opened that is none of Legion's role
  Apps, as `skill://legion-worker/references/review-threads.md` says; the same reference says
  when every thread is settled enough to approve, and resolving one never gates your approval.

A reviewer's phase ends with its completion, not with its review. Every round writes a handoff
and takes this order: write, commit and push the handoff; submit the review of the head that push
made, by its SHA; then complete. An approval waits for the CI verdict to settle green at that head
before you submit it, since an approval stands only on green checks and GitHub can dismiss one
once the head moves, and a verdict that settles red there makes the round's decision a request for
changes naming the failing checks, unless only review workflows the project declares
(`projects.<KEY>.review_workflows`) are red on their own findings: then you answer their threads,
name the ones you accepted to the implementer, who resolves them with its own `gh` as the pull
request's author, and re-run the failed run once they are resolved, as your role prompt says, and
approve once it passes. Any other red required workflow is a failing check like any other. A
request for changes does not wait, since it stands whatever CI says and the issue leaves
reviewing with it. The verdict is of the checks and workflows the base branch requires, the set
READY checks: red when one of them failed, and never red for a check the base branch does not
require. A required check that was cancelled, or that the head's checks
settled without, leaves no verdict until a later settlement decides it, since a run can be
cancelled or not yet queued when the head settles; a required workflow's run on the head that is
still going or has not happened leaves none either. A review of a head the handoff push
then replaces names a head the pull request no longer has. The daemon moves the issue
once both are in — the decision GitHub reports and your completion, in either order — so a
review posted without a completion leaves the issue in reviewing until you finish.

## Retro

- **Retro's commits do not void the reviewer's approval.** Retro's commits above the approved
  head, its `docs/solutions/` learnings and its last commit removing `.legion/<issue>/`
  (`skill://legion-retro` gives the steps), leave the approval standing, and the tree goes to the
  merger — never back to the tester or reviewer. Anything else above the approved head does void
  it, and the merger tells the architect the head must return to review instead of completing. A
  conflict-forced rebase after retro moves those commits with the branch. Every approval moves the
  issue to retro again, so each retro ends with the removal again.
- **Retro brings the PR body's path-derived content up to date before its push.** Whatever the
  repository's instructions derive from the pull request's changed paths (a checklist named for
  each class of path, read by a required check), retro recomputes for the whole diff at its head
  and writes into the live body before its push (`skill://legion-retro`), since the merger
  reports a stale body rather than rewriting it. A body edit changes no commit, so the approval
  stands.

## The merger

- The merger resolves no review thread: the reviewer's approval already judged them, and READY
  requires no thread state beyond it. It proves the head is the approved head plus retro's commits
  with two commands. First `cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && jj -R
  "$LEGION_WORKSPACE" diff --from <approved-sha> --to <tip-sha> --summary`, whose output is quoted
  in READY; then the same with the one fileset `'~(docs/solutions | .legion/<issue>)'` appended,
  which must print nothing (jj unions separate path arguments, so two of them leave nothing out).
  *The READY packet* is
  `READY #<n> at <current sha> (approved at <approved sha>) for <KEY> (<pr url>)`
  (the shape `packages/daemon/internal/prompts/roles/merger.md` defines), then the PR body's
  `Outcome:` line and its `Not proven / risk:` value — every bullet under that label joined with
  `; ` on the one READY line, or `none` — quoted from the `## For the reviewer` block at that same
  head (or one line saying the body carries no brief — the packet still goes out), then the
  `--summary` output and the PR body's gate facts. The READY packet names both the implementer's
  and tester's `E2E` lines; a missing one is reported to the architect instead of completing.
- READY is `handoff_complete` with `ready: true` and the packet as its `summary`; the merger runs
  no pre-flight of its own, and its completion reports the commit the workspace stands on. READY's
  checks run in the daemon, which reads the pull request's head on GitHub and refuses, before
  recording anything: `READY_HEAD_CARRIES_HANDOFFS` while the head still carries `.legion/<issue>/`
  (tell the architect, which has you move the issue back to `retro` with `request_backward_move`,
  so the implementer's retro removes it; a refusal saying GitHub's read of the directory failed is
  GitHub's failure, not the head's: complete again); `READY_HEAD_CONFLICTS` while the pull
  request conflicts with its base, named — GitHub can neither merge it nor start pull_request CI
  for it, so the implementer brings the base into the branch with a forward merge; and
  `READY_CHECKS_NOT_GREEN` unless every check the base branch requires has succeeded at that head
  and every workflow its rulesets require has a passing run there — no result for a check, or no
  run of a workflow, means that head's push skipped CI when it should not have; a check still
  running means wait; a failed one is a finding. Report the refusal to the architect; never
  push a commit to make CI run. On success the daemon posts the packet as a `dispatch message` on
  the issue, publishes it to the project's merge queue role when one is set, and says on the issue
  when that role has no live holder; the answer's `note` says when READY was published on a pull
  request a person already merged, whose head can no longer change, or on a base that requires no
  check, which the packet itself also says. A person merges after READY under the repository's
  branch-protection and code-owner rules.

## After the human merge

- **After a human merges, the implementer verifies in production.**
  The daemon starts the implementer again once the merge lands; the implementer watches the
  deploy slot that carries the merge to `production-apply` (or the equivalent publish step),
  drives the changed path in production through the user's own access path, and records the
  observation on the PR and the issue before the architect signs off. A staging pass is not
  this, since a staging gate does not run every resource production does. If the slot fails on
  the change, the implementer owns the fix and the next slot.
  The record has three places: the PR body's `Production:` line, one pull-request comment
  carrying the Legion footer, and a `dispatch message` on the issue — the reviewer and merger
  read GitHub, the architect reads the issue. When the deploy that carries the merge has not
  happened (a shared profile still holding the previous plugin release, a daemon still running
  the previous commit, a slot nobody has run), open a `dispatch ask` that starts with the
  production gap and why it matters, then names the required install or restart step, its risk,
  and outcome-named options. Keep the `Production:` line at `pending <what is missing>`, and
  complete the check once the human answers. Never record a staging pass as the production check,
  and never let the architect sign off on a `pending` line.
