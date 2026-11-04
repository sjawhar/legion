# The merge gate: review, acceptance, retro, the merge, and the production check

Part of `skill://legion-worker`. Read it when you are the reviewer submitting a review or an
approval, the implementer on a look of its closed issue's production check, or the merger merging
the pull request. Every path it cites is in sjawhar/legion.

The order, in full: the tester's evidence green → the reviewer's approval of the head → the
architect's acceptance, in `accepting` → retro → the merger's merge of the head it read, which
closes the issue → the implementer's production check, after the close. Every handoff is a record
on the Dispatch issue, never a commit, so the head that merges carries none. Retro commits
nothing (*Retro*, below), so the approved head is the head that merges, and no Legion check holds
the head that merges to the head the review approved in any case: a repository that wants its
newest push reviewed says so in its own rulesets. A conflict-forced merge goes back to the
reviewer for a confirmation or a new round, as the fingerprint decides (*The reviewer*, below, and
`skill://legion-worker/references/conflicts-and-rewrites.md`).

## The reviewer

- The reviewer reads CI on GitHub and verifies the `Threads` facts and the issue's results there
  and with `dispatch results --issue <issue>` — never from a handoff — then runs
  `task(agent="thermonuclear-deep-review")` and
  `task(agent="thermonuclear-code-quality")` once at that head — the head the round reviews —
  waits for each one's result with `wait`, since each runs as a job of its own, and records the
  verdict from what those results say. The CI it reads is the head's own checks: no handoff is a
  commit, so every push to the branch is work whose head gets GitHub's workflow runs of its own.
  With `<o>/<r>`, `<n>`, `<base>` and `<headRefOid>` from
  `gh pr view <n> --json baseRefName,headRefOid`, two reads. The required set is the
  base branch's rulesets' required checks and its branch protection's, when it has any. Both
  branch endpoints take the base as a URL path, each segment escaped and each `/` kept (a raw `#`
  or `%` reads another branch), built in the same command from the name GitHub returns, never
  pasted or carried over from an earlier command:

  ```bash
  base=$(gh pr view <n> -R <o>/<r> --json baseRefName -q .baseRefName) && \
    base_path=$(jq -rn --arg b "$base" '$b | split("/") | map(@uri) | join("/")') && \
    echo rulesets: && gh api --paginate "repos/<o>/<r>/rules/branches/$base_path" --jq '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context' && \
    echo protection: && gh api "repos/<o>/<r>/branches/$base_path" --jq '.protection.required_status_checks.checks[].context'
  ```

  A read GitHub refuses stops the command there, and is reported to the architect, never guessed.
  The head's check-runs
  (`gh api repos/<o>/<r>/commits/<headRefOid>/check-runs --paginate --jq '.check_runs[] | [.name, .status, .conclusion] | @tsv'`):
  every required name present, `completed` and `success` (GitHub counts `neutral` and `skipped`
  as passing for a required check). A required check
  `queued` or `in_progress`: wait. One `completed` as `cancelled`, or absent: re-run that run's
  failed and cancelled jobs, once, then wait — never the whole run, which runs again every job that
  succeeded too. Find the run and job at the head: the head's runs
  (`gh api 'repos/<o>/<r>/actions/runs?head_sha=<headRefOid>&per_page=100' --jq '.workflow_runs[] | [.id, .name, .event, .status, .conclusion] | @tsv'`),
  then each candidate run's jobs
  (`gh api repos/<o>/<r>/actions/runs/<run-id>/jobs --jq '.jobs[] | [.id, .name, .status, .conclusion] | @tsv'`).
  A cancelled job: `gh run rerun <run-id> --failed`, which GitHub applies to the run's failed and
  cancelled jobs and the jobs that depend on them, and to nothing that succeeded. Never
  `--job <job-id>` alone: under a routing workflow, re-running only the final job reads the
  cancelled results again and stays red. GitHub refuses a re-run while an attempt of that run is
  still in progress: wait for it. When no
  run at the head holds the job, or nothing will run it (a check only a commit status reports, a
  workflow another repository defines), tell the architect and do not approve.
  Approval is refused while any `before_merge` check of the issue's own Verification document has no passing result that is current at the head being reviewed: `REQUEST_CHANGES` naming each such check by its block id, and `dispatch results --issue <issue>` is the read that decides it.
  An issue with no Verification document, or whose Verification document holds no `before_merge` check, is a gap you name in the review body, never a review that asks for changes: say which of the two it is, and tell the architect, who writes the checks. Your own findings and the checks that do exist still decide the review, and on an issue that has none the architect's acceptance is the gate.
  A result is current at that head while the head is the head of an open pull request, or the final head of a merged one, attributed to the check's own issue. A child's pull request never makes a root check's result current, and a child never records the result that proves a root's check. A child that carries part of a root's requirement proves it with a check on the child's own Verification document whose `serves` names that requirement, and that result is current at the child's head. A root's own checks are proven at the root's own head, or on the default branch for an `after_merge` check, in the root's last round.
  Skip the `Thermo` line entirely on a docs-only PR. Submit **one review per round** —
  `REQUEST_CHANGES` when any correctness finding stands, otherwise `APPROVE` of the head you
  reviewed — always named by SHA — carrying every inline comment in that single
  call: `gh api --method POST repos/{owner}/{repo}/pulls/{number}/reviews --input body.json`
  with `commit_id`, `event` (`REQUEST_CHANGES` or `APPROVE`), `body` (with the
  Legion footer), and a `comments[]` array of `{path, line, side, body}`, one entry per
  finding — never one `pr review` call per finding (each submission fires a `pr-review` wake).
  A `COMMENT` decides nothing. After a
  forward merge, compute the fingerprint (*The unchanged-diff check* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`) at the
  `commit_id` of your last submitted review and at the new head. Equal and that review was
  `APPROVE`: submit one more `APPROVE` naming the new head by SHA, its body naming both SHAs
  and the fingerprint — a confirmation, not a round; no thermo pass, no thread pass. Equal and
  that review was `COMMENT` or `REQUEST_CHANGES`: continue that round against the new head;
  nothing restarts. Different, or either run printed no fingerprint: a new round — thermo again,
  one review.
- Answer every thread you opened, and every thread a bot opened that is none of Legion's role
  Apps, as `skill://legion-worker/references/review-threads.md` says; the same reference says
  when every thread is settled enough to approve, and resolving one never gates your approval.
- **Fix a prose-only finding yourself** — wording, a count, a tense, a citation — in your own
  commit on the pull request, and list it in your review. Send the work back only for a finding
  that touches code or behaviour. Push the fix **before** you submit the round's review, and
  review the head that push made. A push above the head you approved leaves the approval
  standing for nothing, since any push may change code: the round goes stuck and your architect
  is told (`ApprovalStands` in `packages/daemon/internal/classify/decisions.go`,
  `stuckApprovedOtherCode` in `packages/daemon/internal/workflow/review.go`, and the cases "the
  approval, then a push" and "a push, then the approval of the head before it" in
  `packages/daemon/internal/workflow/review_test.go`). GitHub runs the branch's workflows on that
  push, as on every push. Wait for that head's required checks to be green, and approve only
  then: nothing but your own read catches a check your prose broke, since the daemon moves the
  issue for no check at all. A red you approve over reaches nobody who will act on it: the tree
  runs on to the merge, where GitHub holds the pull request under the base branch's required
  checks until someone reads them. A red there is a
  `REQUEST_CHANGES` when the code is at fault, and your own commit to fix when your prose broke
  it. The architect reads your prose commits at its acceptance (*The architect's acceptance*,
  below), and that read is no new round.

A reviewer's phase ends with its completion, not with its review. Every round records a handoff
and takes this order: read CI as above; write the handoff and record it with
`dispatch handoff-write`; submit the review of the head, by its SHA; then complete. A red at the
code is a `REQUEST_CHANGES` naming the check, never an approval. A request for changes does not
wait, since it stands whatever CI says and the issue leaves reviewing with it. The approval waits
on nothing either: it ends the round whatever CI says at the head it names, since the round reads
no CI verdict, and a settlement the daemon reads later — at that head or any above it — moves the
issue nowhere. The daemon names each check GitHub reported, with the conclusion GitHub gave it,
in one message to the tree's architect, the worker of the issue's current phase and the
implementer, and judges none of them: a red is the implementer's to fix or re-run, the
architect's to decide on, and GitHub's to hold the merge for. So the reviewer's own read before
it approves is the only read that decides anything. A review that names an older head reviews
code the pull request no longer has. The daemon moves the issue once all three are in — the
decision GitHub reports, your completion, in either order, and GitHub's own answer that the
approved head is one the issue may advance on — so a review posted without a completion leaves
the issue in reviewing until you finish, and an approved head GitHub answers `blocked` for leaves
it there until that answer comes clean.

## The architect's acceptance

- Between the reviewer's approval and retro the issue sits in `accepting` (Dispatch status
  `needs_review`), where no worker is started: the tree's architect reads the evidence (the
  tester's handoff, `dispatch results --issue <issue>` at the approved head and the reviewer's
  approval of the head by SHA; the daemon lets no child hold it; a parent's waits for its last
  round, `skill://legion-architect`, section 4) and accepts with the
  `legion` tool's `sign_off`, which moves the issue to retro, or sends it back to `implementing`
  or `testing` with `request_backward_move` and a reason, which reaches the started worker's task
  as its `Reason:` line and counts a review round. The acceptance comment names each check with
  its result. An issue with no Verification document, or with no `before_merge` check, reaches
  the architect as a gap the tester and the reviewer named and neither refused, so the
  architect's acceptance is the only gate on it: the acceptance comment names that gap and
  decides there, accepting the issue or sending it back with the checks the architect writes,
  which closes no design gate while dispatch://INFRA-175 has not shipped. The architect also
  reads the reviewer's own prose commits here, with the rest of the evidence, and that read is no
  new round. It does not
  accept while the issue holds a finding ask no controller has taken (`findings.md`, "When no
  controller takes it"). The issue's approval is gated by the `before_merge` checks of its own
  Verification document at its own head, so a child's result against a root's check proves
  nothing for the root: the root's checks are proven at the root's own head, in its last round.
  The acceptance asks for no `after_merge` result: that one is recorded
  after the close (*The production check after the close*, below).
  CI red at the head in `accepting` moves nothing, as it moves nothing anywhere: the daemon names
  the checks GitHub reported and leaves the decision to the roles it told. Read the head's checks
  on GitHub yourself before you accept, and send a red head back with `request_backward_move`.

## Retro

- **Retro commits nothing.** It reads the evidence, writes what the work taught as a
  `dispatch comment` on the issue for the skill librarian, brings the PR body up to date and
  completes (`skill://legion-retro` gives the steps), so the approved head is the head the merger
  merges. Nothing in Legion compares that head with the one the approval named either: a
  repository that wants a push made after the approval reviewed says so in its own rulesets (a
  stale review dismissed, a review required again), and GitHub holds the merge to them. A
  conflict merge pushed above the approved head still goes back to the reviewer for a
  confirmation or a new round, as the fingerprint decides
  (`skill://legion-worker/references/conflicts-and-rewrites.md`), because the branch's own
  procedure says so and not because any Legion check refuses the head. Retro removes nothing.
- **Retro brings the PR body's path-derived content up to date before it completes.** Whatever the
  repository's instructions derive from the pull request's changed paths (a checklist named for
  each class of path, read by a required check), retro recomputes for the whole diff at its head
  and writes into the live body before it completes (`skill://legion-retro`), since the merger
  reports a stale body rather than rewriting it. A body edit changes no commit, so the approval
  stands.

## The merger

- The merger resolves no review thread: the reviewer's approval already judged them, and the
  merge requires no thread state beyond it. It writes no file, commits nothing and pushes
  nothing.
- **The merge is the merger's own act, at the head it read.** It reads the pull request
  (`gh pr view <n> --json number,title,url,state,baseRefName,headRefOid,mergeStateStatus`), and
  every step below names that `headRefOid`:
  - *The message, before the merge.* GitHub's default squash message,
    `squash_merge_commit_message` of `COMMIT_MESSAGES`, joins every commit message of the pull
    request, which puts the tree's `Dispatch:` keys and its `Omp-Session:` trailers on the
    default branch. So the merger writes the message itself,
    `--subject '<pull request title> (#<n>)' --body ''`, on every merge, whatever the repository's
    setting says. It reads that setting first all the same
    (`gh api "repos/<o>/<r>" --jq '[.squash_merge_commit_title, .squash_merge_commit_message] | @tsv'`),
    because the merge-queue path below can pass no message.
  - *The merge.*
    `gh pr merge <n> --squash --match-head-commit <head> --subject '<pull request title> (#<n>)' --body ''`,
    as the implement App, so
    GitHub refuses the merge once the head has moved under it: merging at a named head is how bors
    and Prow's Tide merge only what they tested. On a base with a merge queue — a `merge_queue`
    rule among the base branch's rules — it enqueues the pull request instead, with GraphQL
    `enqueuePullRequest` and the same head as `expectedHeadOid`, because `gh pr merge` on such a
    base turns auto-merge on while a required check is pending, which arms the pull request rather
    than queueing it. `enqueuePullRequest` takes no message, so where that base's
    `squash_merge_commit_message` reads `COMMIT_MESSAGES` the queue would commit the branch's
    messages and nothing the merger passes can stop it: it enqueues nothing, completes nothing,
    and tells the architect, naming the setting and that only a person can change it to `PR_BODY`
    or `BLANK`. It never turns auto-merge on; the daemon submits no merge and changes no
    merge setting of any pull request.
  - *The watch.* On a merge-queue base it reads the pull request until GitHub merges it or takes
    it out of the queue, and acts on a removal: a head the queue took out over its own
    `merge_group` checks is work to fix, so the issue goes back; a person's dequeue is that
    person's decision, reported to the architect. A removal is never a merge.
  - *A failed merge is the merger's decision.* Wait for what GitHub waits for (a required check
    still running, a required review only a person can give) and merge again; merge again at the
    head it re-reads when a push moved it, or when GitHub refused while it was still computing
    mergeability; send the issue back with `request_backward_move` when the head itself must
    change (a conflict with the base, a check that failed); or tell the architect what no
    worker can do — a repository setting or an App permission only a person can change — with
    GitHub's own answer quoted. The repository's required reviews and checks decide what may
    merge: Legion bypasses none, and a review only a person can give is waited for.
- *The completion* is `handoff_complete` with the ordinary two-sentence `summary`, which names the
  pull request, the head merged and the merge commit. It records the merger's phase row and moves
  nothing: no Dispatch message, no publish, no phase. What moves the issue is the daemon's own
  reading of the merged pull request, which writes `done` — before the completion or after it,
  whichever comes second. The daemon refuses that completion only as it refuses any phase
  worker's: `HANDOFF_NOT_CURRENT_PHASE` (the issue left `merging`), `HANDOFF_STALE_GENERATION`,
  `HANDOFF_NO_RUN`, `HANDOFF_ALREADY_RECORDED` and `TREE_LINGERING`. It reads neither the head,
  nor its checks, nor the approval: every one of those reads is the merger's own, above.

## The production check after the close

- **The merge closes the issue; the production check is the implementer's work after the close.**
  When the pull request merges, the daemon reads that merge, writes `done` and, in the
  implementer's session, still live, hands it the check task. On a root whose tree still runs the
  close, and so the check task, comes later: the merge is recorded, the root stays in `merging` on
  its merged pull request until the last other issue of its tree is done or parked, and the check
  opens with that close. Its first line is
  `Production check of <KEY>: <title>.`, and it names the repository, the pull request, the merge
  last. A resumed session tells the check task
  from its last phase task by that first line. The check carries no phase and no handoff: a look
  ends with `handoff_complete` carrying a `verdict` and a `summary`, no commit, and the daemon
  refuses a report on an issue with no open check (`CHECK_NOT_OPEN`), on a look already ended
  (`CHECK_LOOK_NOT_OPEN`), or with no verdict of `pass`, `pending` or `fail`
  (`CHECK_VERDICT_REQUIRED`).
- **Each look does three things.** Do what the deployment instructions say to reach the deploy:
  when they name an agent or role for it, `envoy_publish` the merge commit to that role's topic
  with `expects_reply: none`, and await no reply. Read whether production carries the merge. Then
  report one of three outcomes:
  - `pass`, once you drove the changed path in production through the user's own access path and
    recorded the result: `dispatch result` against the issue's `after_merge` check, at the merge
    commit, its `observed` ending `woken by <event>`, with the printed line in the PR body's
    `Checks` block and one pull-request comment carrying the Legion footer. A closed issue takes a
    result for an `after_merge` or `default_branch` check and refuses every other write. A staging
    pass is not this, since a
    staging gate does not run every resource production does.
  - `pending`, when production does not carry the merge yet (a shared profile still holding the
    previous release, a daemon still running the previous commit, a deploy nobody has run): the
    summary names what is missing, nothing is recorded, and the check's line in the body stays at
    `- Check <block id> (after_merge): not yet run`.
  - `fail`, for a defect: record the failing result with `dispatch result --verdict fail`, file no
    issue, and take the path
    `skill://legion-worker/references/findings.md` gives it. A defect with a clear fix goes to the
    controller topic, with its evidence comment on the controller's report issue. A defect that
    needs a decision is a `dispatch ask` on the report issue, and you publish its reference to the
    controller topic. Either names this issue, the pull request, the merge commit, what you drove
    and what failed. Name the message or the ask (`dispatch://<REPORT KEY>/ask/<id>`) in the
    result's `observed` and in the summary.
- **Nothing waits in the session for a deploy.** A `pending` ends the look. Where no wake is
  queued yet, the daemon suspends your claim with its session kept, and the issue's Sandbox goes
  with its pod and disk; where a wake arrived while the look was open, the next look starts at once
  on the same pod. Either way the daemon wakes the check (after a suspension a new pod comes back
  on a fresh disk, and your session resumes with a new check task) on Dispatch's
  `issue.deployed` event for the pull request; when the project's `legion.yaml` names the
  repository's deploy workflow (`projects.<KEY>.deploys`), at each completed run of that workflow
  on the default branch whose named step (such as `Apply`) succeeded after the merge, once per such
  job; or, for `sjawhar/legion`, on a Legion daemon's boot on a build containing the merge, on
  Dispatch's `server.started` announcing that every server process serves such a build, or on the
  build the daemon read from Dispatch at its boot. The woken look's task names the event
  (`Dispatch's issue.deployed: run <id> completed <t>`,
  `the deploy workflow's run: <repo> <workflow> run <id> attempt <n>, job <job name>: <step> succeeded at <t>`,
  `the Legion daemon's boot on build <sha>`, `Dispatch serving build <sha> on every server process`,
  `Dispatch's build <sha>`), and the result you record names it too, so the pull request carries
  the cause. Never poll for the deploy, never wait on a reply, and never ask anyone for approval
  to deploy. Open a `dispatch ask` to a person only when the instructions name nobody for that
  deploy; it starts with the production gap and why it matters, then names the required install or
  restart step, its risk, and outcome-named options, and the look still ends `pending`.
- **The bounds.** A look is bounded at 2 hours: one that reports nothing ends as a `pending`
  would. The whole check is bounded by the project's `production_check_hours` (336, 14 days, by
  default) from the close, and the wake at that bound is the last look, told so in its task
  (`the bound: production did not carry the merge within <N days>`). A `pass` or `fail` there
  closes the check as any look's would; a `pending` closes it as not run, and before reporting it
  you write on the pull request, in the check's line and a comment, that the check did not
  run because production did not carry the merge within the bound.
- **The record lives on the pull request and in the result.** Dispatch refuses a message, a
  comment or an ask on a closed issue (`ISSUE_CLOSED`) and takes only a result for an
  `after_merge` or `default_branch` check, so the deployment instructions' "on the pull request
  and the issue" is the pull request and that result. A defect reaches Dispatch by the two paths above:
  a clear defect as the message to the controller, which gives it to the tree that owns that code,
  and any other as the ask on the
  controller's report issue. The architect accepted the issue before the merge and is asked
  nothing after it. `pass` or
  `fail` closes the check: your claim is retired and the issue's Sandbox goes with its pod and
  disk, for a root as for a child. Neither ending retires or suspends anything of an issue back in
  the workflow (a root reopened inside its linger): its claim and pod are the new run's.
