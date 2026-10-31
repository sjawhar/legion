# The merge gate: review, acceptance, retro, the merge, and the production check

Part of `skill://legion-worker`. Read it when you are the reviewer submitting a review or an
approval, the implementer on a look of its closed issue's production check, or the merger merging
the pull request. Every path it cites is in sjawhar/legion.

The order, in full: the tester's evidence green → the reviewer's approval of the head → the
architect's acceptance, in `accepting` → retro → the merger's merge of the head it read, which
closes the issue → the implementer's production check, after the close. The approved
head carries the issue's handoffs, `.legion/<issue>/`; retro's last commit removes them from the
head that merges (dispatch://LEGION-605), since a squash merge commits that head merged into
the default branch and nothing there reads a handoff, and the merger sends a head that still
carries them back to retro rather than merging it. A conflict-forced merge goes back
to the reviewer for a confirmation or a new round, as the fingerprint decides (*The reviewer*,
below, and `skill://legion-worker/references/conflicts-and-rewrites.md`).

## The reviewer

- The reviewer reads CI on GitHub and verifies the `Threads` and `E2E` facts there directly —
  never from a handoff — then runs `task(agent="thermonuclear-deep-review")` and
  `task(agent="thermonuclear-code-quality")` once at that head — the head the round reviews —
  and records the verdict. The CI it reads is the checks that stand for the code under review:
  those GitHub ran for the newest push of the branch that started `pull_request` workflows, when
  every commit above that push's head changes only `.legion/<issue>/`. GitHub starts no workflow
  run for a head whose commit message carries `skip-checks: true`, which every handoff-only push
  but retro's and the reviewer's does. The reviewer's push never carries the `skip-checks: true`
  line, whatever paths it touches, because the daemon moves the issue to `accepting` only once
  GitHub reports the approved head mergeable, and a base that requires a check answers `blocked`
  for a head GitHub was told to skip; retro's push carries no such line for its own reason, so
  both heads get GitHub's workflow runs of their own. With `<o>/<r>`, `<n>`, `<base>`, `<headRefName>` and `<headRefOid>` from
  `gh pr view <n> --json baseRefName,headRefOid,headRefName`, four reads. The required set is the
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

  A read GitHub refuses stops the command there, and is reported to the architect, never guessed. The CI head, the head of the
  branch's newest `pull_request` run
  (`gh api 'repos/<o>/<r>/actions/runs?branch=<headRefName>&event=pull_request&per_page=30' --jq '.workflow_runs[0].head_sha'`,
  newest first). That commit's check-runs
  (`gh api repos/<o>/<r>/commits/<ci head>/check-runs --paginate --jq '.check_runs[] | [.name, .status, .conclusion] | @tsv'`):
  every required name present, `completed` and `success` (GitHub counts `neutral` and `skipped`
  as passing for a required check). And the compare proving only `.legion/<issue>/` changed since,
  `<issue>` being your `LEGION_ISSUE` (another issue's `.legion/` is code, and a head carrying it
  is not one you may treat as handoffs-only)
  prints `handoffsOnly` `true`; `false` means code was pushed since CI ran, so wait for the new
  push's runs and read the CI head again). GitHub's compare lists at most 300 files for the whole
  comparison, so `files` at 300 is a comparison GitHub cut at its cap and `handoffsOnly` decides
  nothing: tell the architect and do not approve. A required check
  `queued` or `in_progress`: wait. One `completed` as `cancelled`, or absent: re-run that run's
  failed and cancelled jobs, once, then wait — never the whole run, which runs again every job that
  succeeded too. Find the run and job at the CI head: the head's runs
  (`gh api 'repos/<o>/<r>/actions/runs?head_sha=<ci head>&per_page=100' --jq '.workflow_runs[] | [.id, .name, .event, .status, .conclusion] | @tsv'`),
  then each candidate run's jobs
  (`gh api repos/<o>/<r>/actions/runs/<run-id>/jobs --jq '.jobs[] | [.id, .name, .status, .conclusion] | @tsv'`).
  A cancelled job: `gh run rerun <run-id> --failed`, which GitHub applies to the run's failed and
  cancelled jobs and the jobs that depend on them, and to nothing that succeeded. Never
  `--job <job-id>` alone: under a routing workflow, re-running only the final job reads the
  cancelled results again and stays red. GitHub refuses a re-run while an attempt of that run is
  still in progress: wait for it. When no
  run at the CI head holds the job, or nothing will run it (a check only a commit status reports, a
  workflow another repository defines), tell the architect and do not approve. `gh pr checks <n>`
  is not the read: it reads only the current head, which
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
  review the head that push made. A change pushed above the head you approved leaves the approval
  standing for nothing: the round goes stuck and your architect is told (`ApprovalStands` in
  `packages/daemon/internal/classify/decisions.go`, `stuckApprovedOtherCode` in
  `packages/daemon/internal/workflow/review.go`, and the cases "the approval, then a code push"
  and "a push since the approved head changed code, then an approval of the head" in
  `packages/daemon/internal/workflow/review_test.go`). That push carries no `skip-checks: true`
  line, as no push of the reviewer's does, so GitHub runs the branch's workflows on it. Wait for
  that head's required checks to be green, and approve only then: nothing but your own read
  catches a check your prose broke, since the daemon moves the issue for no check at all. A red
  you
  approve over reaches nobody who will act on it: the tree runs on to the merge, where GitHub
  holds the pull request under the base branch's required checks until someone reads them. A red
  there is a
  `REQUEST_CHANGES` when the code is at fault, and your own commit to fix when your prose broke
  it. The architect reads your prose commits at its acceptance (*The architect's acceptance*,
  below), and that read is no new round.

A reviewer's phase ends with its completion, not with its review. Every round writes a handoff
and takes this order: read CI as above; write, commit and push the handoff; submit the review of
the head that push made, by its SHA; then complete. A red at the code is a `REQUEST_CHANGES`
naming the check, never an approval. A request for changes does not wait, since it stands
whatever CI says and the issue leaves reviewing with it. The approval waits on nothing at the
reviewer's own handoff commit either: it ends the round whatever CI says there, since the round
reads no CI verdict, and a settlement the daemon reads later — at that commit or any above it —
moves the issue nowhere. The daemon names each check GitHub reported, with the conclusion GitHub
gave it, in one message to the tree's architect, the worker of the issue's current phase and the
implementer, and judges none of them: a red is the implementer's to fix or re-run, the architect's
to decide on, and GitHub's to hold the merge for. So the reviewer's own read before it approves is
the only read that decides anything. A review of a head the handoff push then replaces names a
head the pull request no longer has. The daemon moves the issue once all three are in — the
decision GitHub reports, your completion, in either order, and GitHub's own answer that the
approved head is one the issue may advance on — so a review posted without a completion leaves the
issue in reviewing until you finish, and an approved head GitHub answers `blocked` for leaves it
there until that answer comes clean. The reviewer's push never carries the `skip-checks: true`
line, whatever paths it touches, because the daemon moves the issue to `accepting` only once
GitHub reports the approved head mergeable, and a base that requires a check answers `blocked` for
a head GitHub was told to skip.

## The architect's acceptance

- Between the reviewer's approval and retro the issue sits in `accepting` (Dispatch status
  `needs_review`), where no worker is started: the tree's architect reads the evidence (the
  tester's handoff, the PR body's `E2E (implementer)` and `E2E (tester)` lines and the reviewer's
  approval of the head by SHA; the daemon lets no child hold it; a parent's waits for its last
  round, `skill://legion-architect`, section 4) and accepts with the
  `legion` tool's `sign_off`, which moves the issue to retro, or sends it back to `implementing`
  or `testing` with `request_backward_move` and a reason, which reaches the started worker's task
  as its `Reason:` line and counts a review round. The architect also reads the reviewer's own
  prose commits here, with the rest of the evidence, and that read is no new round. It does not
  accept while the issue holds a finding ask no controller has taken (`findings.md`, "When no
  controller takes it"). The acceptance asks no `Production:` line of anyone: that line is written
  after the close (*The production check after the close*, below).
  CI red at the head in `accepting` moves nothing, as it moves nothing anywhere: the daemon names
  the checks GitHub reported and leaves the decision to the roles it told. Read the head's checks
  on GitHub yourself before you accept, and send a red head back with `request_backward_move`.

## Retro

- **Retro's commits do not void the reviewer's approval.** Retro's commits above the approved
  head, its `docs/solutions/` learnings and its last commit removing `.legion/<issue>/`
  (`skill://legion-retro` gives the steps), leave the approval standing, and the tree goes to the
  merger — never back to the tester or reviewer. Nothing in Legion compares the head that merges
  with the one the approval named: a repository that wants a push made after the approval reviewed
  says so in its own rulesets (a stale review dismissed, a review required again), and GitHub
  holds the merge to them. A conflict merge pushed above the approved head still goes back to the
  reviewer for a confirmation or a new round, as the fingerprint decides
  (`skill://legion-worker/references/conflicts-and-rewrites.md`), because the branch's own
  procedure says so and not because any Legion check refuses the head. Every approval goes through
  the architect's acceptance to retro again, so each retro ends with the removal again.
- **Retro brings the PR body's path-derived content up to date before its push.** Whatever the
  repository's instructions derive from the pull request's changed paths (a checklist named for
  each class of path, read by a required check), retro recomputes for the whole diff at its head
  and writes into the live body before its push (`skill://legion-retro`), since the merger
  reports a stale body rather than rewriting it. A body edit changes no commit, so the approval
  stands.

## The merger

- The merger resolves no review thread: the reviewer's approval already judged them, and the
  merge requires no thread state beyond it. It writes no file, commits nothing and pushes
  nothing.
- **The merge is the merger's own act, at the head it read.** It reads the pull request
  (`gh pr view <n> --json number,title,url,state,baseRefName,headRefOid,mergeStateStatus`), and
  every step below names that `headRefOid`:
  - *The handoffs first.* While that head still carries `.legion/<issue>/`
    (`gh api 'repos/{owner}/{repo}/contents/.legion/<issue>?ref=<head>'`: GitHub's 404 means it
    carries none, and any other failure leaves it unknown), it merges nothing and completes
    nothing. Retro's last commit is what removes the directory, so a head carrying it is one retro
    did not finish, and a squash merge would put this issue's handoffs on the default branch.
    Nothing in the daemon reads the head for it, so this read is the only one. It
    tells the architect, who moves the issue back to `retro` with `request_backward_move`, and it
    merges the head retro's push then makes.
  - *The message, before the merge.* GitHub's default squash message,
    `squash_merge_commit_message` of `COMMIT_MESSAGES`, joins every commit message of the pull
    request, which puts the tree's `Dispatch:` keys, its `Omp-Session:` trailers and every
    `skip-checks: true` line on the default branch. So the merger writes the message itself,
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
  nor its checks, nor the approval, nor `.legion/<issue>/`: every one of those reads is the
  merger's own, above.

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
    wrote the record: the PR body's `Production:` line,
    `<what was checked in production, how, what was observed> — merge commit <sha>; woken by <event>`,
    and one pull-request comment carrying the Legion footer. A staging pass is not this, since a
    staging gate does not run every resource production does.
  - `pending`, when production does not carry the merge yet (a shared profile still holding the
    previous release, a daemon still running the previous commit, a deploy nobody has run): the
    summary names what is missing, and the `Production:` line stays at
    `pending deploy of <merge sha>`.
  - `fail`, for a defect: file no issue, and take the path
    `skill://legion-worker/references/findings.md` gives it. A defect with a clear fix goes to the
    controller topic, with its evidence comment on the controller's report issue. A defect that
    needs a decision is a `dispatch ask` on the report issue, and you publish its reference to the
    controller topic. Either names this issue, the pull request, the merge commit, what you drove
    and what failed. Name the message or the ask (`dispatch://<REPORT KEY>/ask/<id>`) in the
    `Production:` line and in the summary.
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
  `Dispatch's build <sha>`), and its `Production:` line names it too, so the pull request carries
  the cause. Never poll for the deploy, never wait on a reply, and never ask anyone for approval
  to deploy. Open a `dispatch ask` to a person only when the instructions name nobody for that
  deploy; it starts with the production gap and why it matters, then names the required install or
  restart step, its risk, and outcome-named options, and the look still ends `pending`.
- **The bounds.** A look is bounded at 2 hours: one that reports nothing ends as a `pending`
  would. The whole check is bounded by the project's `production_check_hours` (336, 14 days, by
  default) from the close, and the wake at that bound is the last look, told so in its task
  (`the bound: production did not carry the merge within <N days>`). A `pass` or `fail` there
  closes the check as any look's would; a `pending` closes it as not run, and before reporting it
  you write on the pull request, in the `Production:` line and a comment, that the check did not
  run because production did not carry the merge within the bound.
- **The record lives on the pull request alone.** Dispatch refuses a message, a comment or an ask
  on a closed issue (`ISSUE_CLOSED`), so the deployment instructions' "on the pull request and the
  issue" is the pull request for a closed issue. A defect reaches Dispatch by the two paths above:
  a clear defect as the message to the controller, which gives it to the tree that owns that code,
  and any other as the ask on the
  controller's report issue. The architect accepted the issue before the merge and is asked
  nothing after it. `pass` or
  `fail` closes the check: your claim is retired and the issue's Sandbox goes with its pod and
  disk, for a root as for a child. Neither ending retires or suspends anything of an issue back in
  the workflow (a root reopened inside its linger): its claim and pod are the new run's.
