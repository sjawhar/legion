# The merge gate: review, retro, READY, and the production check

Part of `skill://legion-worker`. Read it when you are the reviewer submitting a review or an
approval, the implementer recording the production check, or the merger building READY and
submitting the merge. Every path it cites is in sjawhar/legion.

The order, in full: the tester's evidence green → the reviewer's approval of the head →
retro → the merger's READY → the merge → the implementer's production check. The approved
head carries the issue's handoffs, `.legion/<issue>/`; retro's last commit removes them from the
head that merges (dispatch://LEGION-605), since a squash merge commits that head merged into
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
  and records the verdict. The `CI` fact is the checks that stand for the code under review:
  those GitHub ran for the newest push of the branch that started `pull_request` workflows, when
  every commit above that push's head changes only `.legion/` (a push that changes only
  `.legion/` carries `skip-checks: true`, so GitHub runs no workflow for it). With `<o>/<r>`,
  `<n>`, `<base>`, `<headRefName>` and `<headRefOid>` from
  `gh pr view <n> --json baseRefName,headRefOid,headRefName`, four reads. The required set, from
  the base branch's rulesets
  (`gh api repos/<o>/<r>/rules/branches/<base> --jq '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context'`)
  and its branch protection, when it has any
  (`gh api repos/<o>/<r>/branches/<base> --jq '.protection.required_status_checks.checks[].context'`);
  a read GitHub refuses is reported to the architect, never guessed. The CI head, the head of the
  branch's newest `pull_request` run
  (`gh api 'repos/<o>/<r>/actions/runs?branch=<headRefName>&event=pull_request&per_page=30' --jq '.workflow_runs[0].head_sha'`,
  newest first); the PR body's `CI:` line names runs at that same commit
  (`gh run view <run-id> --json headSha,status,conclusion`), or the line is stale and the review
  says so. That commit's check-runs
  (`gh api repos/<o>/<r>/commits/<ci head>/check-runs --paginate --jq '.check_runs[] | [.name, .status, .conclusion] | @tsv'`):
  every required name present, `completed` and `success` (GitHub counts `neutral` and `skipped`
  as passing for a required check). And the compare proving only `.legion/` changed since
  (`gh api repos/<o>/<r>/compare/<ci head>...<headRefOid> --jq '[.files[].filename] | all(startswith(".legion/"))'`
  prints `true`; `false` means code was pushed since CI ran, so wait for the new push's runs and
  read the CI head again). A required check `queued` or `in_progress`: wait. One `completed` as
  `cancelled`, or absent: `gh run rerun <run-id>` once, then wait; when nothing will run it (a
  check only a commit status reports, a workflow another repository defines), tell the architect
  and do not approve. `gh pr checks <n>` is not the read: it reads only the current head, which
  at a handoff head shows only the `pr-title` runs the review events start (measured on
  sjawhar/legion#1944 at 0220b94fc2 on 2026-10-14).
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
and takes this order: read CI as above; write, commit and push the handoff; submit the review of
the head that push made, by its SHA; then complete. A red at the code is a `REQUEST_CHANGES`
naming the check, never an approval. A request for changes does not wait, since it stands
whatever CI says and the issue leaves reviewing with it. The approval waits on nothing at the
handoff commit either: it ends the round whatever CI says there, since the round reads no CI
verdict, and a red the daemon reads later — at that commit or any above it — is the code's red
and resumes the implementer from wherever the issue is (reviewing, retro or merging) without the
reviewer. A review of a head the handoff push then replaces names a head the pull request no
longer has. The daemon moves the issue once both are in — the decision GitHub reports and your
completion, in either order — so a review posted without a completion leaves the issue in
reviewing until you finish.

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
  recording anything, on each ground the merger's role prompt names in its `Go daemon workflow`
  part, by its code and with its remedy. That part is the one complete list of READY's refusals.
  A refusal records nothing; the merger reports it to the architect unless it says to complete
  again, and never pushes a commit to make CI run. On success the daemon posts the packet as a
  `dispatch message` on the issue, publishes it to the project's merge queue role when one is set,
  and says on the issue when that role has no live holder; the answer's `note` says when READY was
  published on a pull request a person already merged, whose head can no longer change, or on a
  base that requires no check, which the packet itself also says, or carries the by-hand note of a
  pull request a person merges.
- **The merger submits the merge the moment READY is accepted, unless the answer says a person
  merges it.** In the same turn, before anything else, it runs
  `gh pr merge <n> -R <owner>/<repo> --auto --squash --match-head-commit <head>` with its pane's
  plain `gh`: `<n>` the pull request number, `<head>` the full sha of the current head READY named.
  A repository with a merge queue enqueues the pull request; one without arms auto-merge; one whose
  rules are already satisfied merges at once. `--match-head-commit` has GitHub refuse the request
  when the head has moved since READY read it. The repository's required reviews and checks decide
  when it lands: the merger makes no merge by hand and bypasses no rule. It then tells the
  architect with `envoy_publish` to its encoded role token what the command answered — enqueued,
  auto-merge enabled, or merged — and the head. When the answer's `note` begins
  `MANUAL_MERGE_REQUIRED:` (the pull request touches a path the project's `merge.by_hand` names),
  READY stands but the merger submits nothing: it tells the architect the note and the head, and a
  person merges under the repository's rules. The daemon tells that READY with the note at its head
  and the packet's `Merge:` line replaced, so the issue and the merge queue role are never asked to
  arm it. On any READY refusal it submits nothing, since an armed request would merge through the
  gate the refusal names; a READY the design gate refused is submitted later, on the architect's
  word, once the gate opens, and that turn runs only `gh` and a read of the READY the daemon posted
  then: a refusal or a posted READY carrying the by-hand note is submitted by nobody. A push that
  moves the head past a READY the gate holds voids it before then: the architect is told
  `READY_HEAD_MOVED` and has the merger build a new READY at the new head. A
  `gh pr merge` GitHub refuses is reported to the architect with its message; a person can still
  merge after READY. The packet's second line records the step,
  `Merge: submit on normal acceptance with gh pr merge <n> -R <owner>/<repo> --auto --squash --match-head-commit <head>`.
- **The daemon disarms a merge no READY covers.** Auto-merge stays enabled across pushes, and
  `--match-head-commit` is checked only when it is enabled, so the daemon itself disables the pull
  request's auto-merge and takes it out of a merge queue, as the implement App, on any push or
  reopen but one landing on an awaiting READY's head, when READY is withdrawn (a push or reopen
  moved the head past it, its CI turned red, or it conflicts with its base, each of which sends the
  issue back to `implementing`), when a child is set back to `todo`, and when the issue leaves the
  workflow. A by-hand READY covers no arm, so whoever arms it is undone. READY waits
  (`READY_DISARM_PENDING`) until that disarm has finished. No role runs a disarm.

## After the merge

- **After the merge, the implementer verifies in production.**
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
  the previous commit, a slot nobody has run) and you cannot perform it yourself, ask the agent or
  role the deployment instructions name for that deploy: `envoy_publish` to its role topic with the
  merge commit and `expects_reply: required`, then wait on the production signal the instructions
  name. Open a `dispatch ask` to a person only when the instructions name nobody for that deploy;
  it starts with the production gap and why it matters, then names the required install or restart
  step, its risk, and outcome-named options. Never ask anyone for approval to deploy. Keep the
  `Production:` line at `pending <what is missing>`, and complete the check once that signal shows
  the deploy carries the merge (or the person answers, when an ask was the route). Never record a
  staging pass as the production check, and never let the architect sign off on a `pending` line.
