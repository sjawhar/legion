# The merge gate: review, acceptance, retro, READY, the merge, and the production check

Part of `skill://legion-worker`. Read it when you are the reviewer submitting a review or an
approval, the implementer on a look of its closed issue's production check, or the merger
building READY. Every path it cites is in sjawhar/legion.

The order, in full: the tester's evidence green → the reviewer's approval of the head → the
architect's acceptance, in `accepting` → retro → the merger's READY → the merge, which closes the
issue → the implementer's production check, after the close. The approved
head carries the issue's handoffs, `.legion/<issue>/`; retro's last commit removes them from the
head that merges (dispatch://LEGION-605), since a squash merge commits that head merged into
the default branch and nothing there reads a handoff, and READY refuses a head that still
carries them. After the approval, only retro's commits leave it standing on their own: those that
change only `docs/solutions/`, and that removal (*Retro*, below). A conflict-forced merge goes back
to the reviewer for a confirmation or a new round, as the fingerprint decides (*The reviewer*,
below, and `skill://legion-worker/references/conflicts-and-rewrites.md`), unless READY refused
it, and any other change to the head voids it.

## The reviewer

- The reviewer reads CI on GitHub and verifies the `Threads` and `E2E` facts there directly —
  never from a handoff — then runs `task(agent="thermonuclear-deep-review")` and
  `task(agent="thermonuclear-code-quality")` once at that head — the head the round reviews —
  and records the verdict. The CI it reads is the checks that stand for the code under review:
  those GitHub ran for the newest push of the branch that started `pull_request` workflows, when
  every commit above that push's head changes only `.legion/` (GitHub starts no workflow run for a
  head whose commit message carries `skip-checks: true`, which every handoff-only push before
  retro does; retro's push carries no such line, so its head does get GitHub's workflow runs of
  its own). With `<o>/<r>`, `<n>`, `<base>`, `<headRefName>` and `<headRefOid>` from
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
  as passing for a required check). And the compare proving only `.legion/` changed since
  (`gh api repos/<o>/<r>/compare/<ci head>...<headRefOid> --jq '{handoffsOnly: ([.files[].filename] | all(startswith(".legion/"))), files: (.files | length)}'`
  prints `handoffsOnly` `true`; `false` means code was pushed since CI ran, so wait for the new
  push's runs and read the CI head again). GitHub's compare lists at most 300 files for the whole
  comparison, so `files` at 300 is a comparison GitHub cut at its cap and `handoffsOnly` decides
  nothing: tell the architect and do not approve. A required check
  `queued` or `in_progress`: wait. One `completed` as
  `cancelled`, or absent: `gh run rerun <run-id>` once, then wait; when nothing will run it (a
  check only a commit status reports, a workflow another repository defines), tell the architect
  and do not approve. `gh pr checks <n>` is not the read: it reads only the current head, which
  at a handoff head shows only what review and comment events start, the `public-text-scan` run
  and the `public-text` status (measured with `pr-title`, which review events then started, on
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
  one review. A return whose task's `Reason:` names a READY refusal is never a confirmation, even
  after a forward merge, and a no-fix `REQUEST_CHANGES` that answered one is never continued: the round
  after it reviews the refused change in full from the last `APPROVE` (*Retro's commits do not
  void the reviewer's approval* below).
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
  line, so GitHub runs the branch's workflows on it. Wait for that head's required checks to be
  green, and approve only then. The daemon sends nothing back for a red at a head the review App
  pushed (`PlannedRed` in `classify/decisions.go`), so your own read is the only one. A red you
  approve over reaches nobody: the tree runs on to the merge, where GitHub holds the pull request
  under the base branch's required checks until someone reads them. A red there is a
  `REQUEST_CHANGES` when the code is at fault, and your own commit to fix when your prose broke
  it. The architect reads your prose commits at its acceptance (*The architect's acceptance*,
  below), and that read is no new round. A round after a READY refusal is the one exception. Its
  approval needs the tester's `E2E` line to name the head with only `.legion/` changed since. So
  there you fix no prose yourself, and you name each prose finding in a `REQUEST_CHANGES`.

A reviewer's phase ends with its completion, not with its review. Every round writes a handoff
and takes this order: read CI as above; write, commit and push the handoff; submit the review of
the head that push made, by its SHA; then complete. A red at the code is a `REQUEST_CHANGES`
naming the check, never an approval. A request for changes does not wait, since it stands
whatever CI says and the issue leaves reviewing with it. The approval waits on nothing at a
handoff-only commit either: it ends the round whatever CI says there, since the round reads no CI
verdict, and a red the daemon reads later — at that commit or any above it — is the code's red
and resumes the implementer from wherever the issue is (reviewing, retro or merging) without the
reviewer. A head your own push changed beyond `.legion/` is the exception. The daemon marks it a
planned red and sends nothing back for it, so wait for its checks yourself (*Fix a prose-only
finding yourself*, above). A review of a head the handoff push then replaces names a head the pull
request no longer has. The daemon moves the issue once both are in — the decision GitHub reports
and your completion, in either order — so a review posted without a completion leaves the issue in
reviewing until you finish.

## The architect's acceptance

- Between the reviewer's approval and retro the issue sits in `accepting` (Dispatch status
  `needs_review`), where no worker is started: the tree's architect reads the evidence (the
  tester's handoff, the PR body's `E2E (implementer)` and `E2E (tester)` lines and the reviewer's
  approval of the head by SHA; children still running do not hold it) and accepts with the
  `legion` tool's `sign_off`, which moves the issue to retro, or sends it back to `implementing`
  or `testing` with `request_backward_move` and a reason, which reaches the started worker's task
  as its `Reason:` line and counts a review round. The architect also reads the reviewer's own
  prose commits here, with the rest of the evidence, and that read is no new round. It does not
  accept while the issue holds a finding ask no controller has taken (`findings.md`, "When no
  controller takes it"). The acceptance asks no `Production:` line of anyone: that line is written
  after the close (*The production check after the close*, below).
  CI red at the head in `accepting` returns the issue to `implementing` as it does in review. A
  head the review App pushed beyond `.legion/` is the exception: the daemon marks it a planned red
  and acts on it nowhere, and GitHub holds the merge under the base branch's own rules.

## Retro

- **Retro's commits do not void the reviewer's approval.** Retro's commits above the approved
  head, its `docs/solutions/` learnings and its last commit removing `.legion/<issue>/`
  (`skill://legion-retro` gives the steps), leave the approval standing, and the tree goes to the
  merger — never back to the tester or reviewer. Anything else above the approved head does void
  it: READY is refused `READY_UNREVIEWED_CHANGE`, naming the paths, and the merger tells the
  architect, who has it send the head back to review instead of completing, the move's reason
  naming the refusal's code and what it found. A conflict merge pushed above the approved head
  with no review round after it brings the base's paths above that head, so READY refuses it the
  same way; one sent back through `implementing` by the conflict procedure gets its confirmation
  before READY runs. A READY refusal's return reaches the reviewer as a new round, never a
  fingerprint confirmation. Every approval goes through the architect's acceptance to retro
  again, so each retro ends with the removal again.
- **Retro brings the PR body's path-derived content up to date before its push.** Whatever the
  repository's instructions derive from the pull request's changed paths (a checklist named for
  each class of path, read by a required check), retro recomputes for the whole diff at its head
  and writes into the live body before its push (`skill://legion-retro`), since the merger
  reports a stale body rather than rewriting it. A body edit changes no commit, so the approval
  stands.

## The merger

- The merger resolves no review thread: the reviewer's approval already judged them, and READY
  requires no thread state beyond it. It quotes what sits above the approved head with
  `cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && jj -R
  "$LEGION_WORKSPACE" diff --from <approved-sha> --to <tip-sha> --summary` in READY; the daemon,
  not the merger, holds those commits to retro's: READY reads GitHub's compare of the round's
  approved head with READY's head and refuses any other path (`READY_UNREVIEWED_CHANGE`), and a
  round with no recorded approval (`READY_NOT_APPROVED`).
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
  part, by its code and with its remedy. That part is the one complete list of READY's refusals. A
  refusal records nothing; the merger reports it to the architect unless it says to complete
  again, and never pushes a commit to make CI run. On success the daemon posts the packet as a
  `dispatch message` on the issue, publishes it to the project's merge queue role when one is set,
  and says on the issue when that role has no live holder; the answer's `note` says when READY was
  published on a pull request a person already merged, whose head can no longer change, or carries
  the by-hand note of a pull request a person merges.
- **The daemon submits the merge the moment READY is accepted, unless the answer says a person
  merges it.** It queues a `ready_merge` outbox row for the head READY named, and the row, as the
  implement App, reads GitHub and then the record: while the issue still awaits that READY head, a
  base with a merge queue gets the pull request enqueued once GitHub reports it mergeable now
  (`CLEAN`, `UNSTABLE` or `HAS_HOOKS`); otherwise a pull request GitHub reports mergeable now is
  squash-merged, and one in a repository that allows auto-merge is armed. Every request carries
  READY's head as GitHub's expected head, so GitHub refuses it once the head has moved. On a
  merge-queue base the row never arms auto-merge: while a required check still runs, or anything
  else keeps GitHub from reporting the pull request mergeable, it waits, reading GitHub again on
  the outbox's backoff, and enters the queue once GitHub does. In a repository that allows neither a
  merge queue nor auto-merge, the row waits the same way until GitHub reports the pull request
  mergeable — a required check finishing, a required review a person gives — and then merges it. An
  arm or a queue entry is not the end: the row keeps reading GitHub until GitHub reports the pull
  request merged at READY's head. Once it has seen the arm or entry, it never submits that head
  again after someone else removes it: the merge queue taking the pull request out because the merge
  failed (GitHub's reason `failed_checks`, or an unexplained reason with a check that failed, timed
  out, failed to start, needed action or went stale on the commit the queue built for its
  `merge_group` CI) sends the issue back to `implementing`, the implementer's task naming each
  failing check and its run, as a red head does; any other removal (a person dequeues it,
  cancelling its runs, or turns auto-merge off, GitHub turns it off, the queue finds a conflict)
  ends the row and is told to the architect once
  (`merge-removed`). Two removals are submitted again: the implement App's own disarm sent before
  READY that GitHub applied late (auto-merge `manually_disabled`, or a `manual` dequeue, by that
  App), and auto-merge a close turned off, which the reopen at READY's head undid. GitHub's
  rules decide each answer: the daemon bypasses
  none, and a person may merge it first. A refusal a retry would meet again (squash merges disabled,
  a permission the App lacks) ends the row and is told to the architect once (`merge-refused`), with
  GitHub's message. While the record still awaits READY's head, GitHub showing the pull request
  closed or at another head is a webhook not yet applied, so the row waits for the two to agree.
  Only once GitHub's last change to the pull request is more than 10 minutes old, however long the
  row waited before, does the row end and tell the architect once (`merge-stalled`). A merge the
  daemon submitted that GitHub has not completed within an hour of READY's acceptance tells the
  issue's architect once (`merge-waiting`) and goes on waiting: nothing is withdrawn, the merge
  stands, the issue stays in `awaiting_merge`, and the architect reads the pull request for what
  GitHub waits on — a required check that never ran or failed, a review only a person can give, a
  queue that has not landed it — and asks a person for what no worker can do. A reopen at
  READY's head reuses the row, starts it over as though nothing had been armed, and takes it from a
  run that read the record before the reopen. Any other failure retries.
  No role runs `gh pr merge`. When the answer's `note` begins `MANUAL_MERGE_REQUIRED:` (the pull
  request touches a path the project's `merge.by_hand` names), READY stands but the daemon submits
  nothing: the merger tells the architect the note and the head, and a person merges under the
  repository's rules. While an earlier auto-merge disarm for that pull request remains unfinished,
  a by-hand READY refuses `READY_DISARM_PENDING`; retry it once the disarm finishes, so a stale
  ordinary arm cannot merge the person-only head. Ordinary READY does not wait for the disarm. The
  daemon tells that READY with the note at its head and the packet's `Merge:` line replaced, so the
  issue and the merge queue role are never asked to arm it. A READY the design gate refused is
  submitted by the daemon when the approval that opens the gate moves the issue to
  `awaiting_merge`. A push that moves the head past a READY the gate holds voids it before then:
  the architect is told `READY_HEAD_MOVED` and has the merger build a new READY at the new head.
  The packet's second line records the step,
  `Merge: the daemon submits pull request #<n> at <head> once READY is accepted`.
- **The daemon disarms a merge no READY covers.** Auto-merge stays enabled across pushes, and the
  expected head is checked only when it is enabled, so the daemon itself disables the pull
  request's auto-merge and takes it out of a merge queue, as the implement App, on any push or
  reopen but one landing on an awaiting READY's head, when READY is withdrawn (a push or reopen
  moved the head past it, its CI turned red, or it conflicts with its base, each of which sends the
  issue back to `implementing`), when a child is set back to `todo`, and when the issue leaves the
  workflow. A by-hand READY covers no arm, so whoever arms it is undone. A `ready_merge` row whose
  READY the record has left, or whose pull request the record holds closed, submits nothing; a pull
  request reopened at an awaiting READY's head gets that head's merge submitted again. No role runs
  a disarm.

## The production check after the close

- **The merge closes the issue; the production check is the implementer's work after the close.**
  When the pull request merges, the daemon writes `done` and, in the implementer's session, still
  live, hands it the check task. On a root whose tree still runs the close, and so the check task,
  comes later: the merge is recorded, the root waits in `awaiting_merge` until the last other
  issue of its tree is done or parked, and the check opens with that close. Its first line is
  `Production check of <KEY>: <title>.`, and it names the repository, the pull request, the merge
  commit, the look's number, what woke the look (`the merge`, for the first) and whether it is the
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
  Dispatch's `server.started` naming such a build, or on the build the daemon read from Dispatch at
  its boot. The woken look's task names the event (`Dispatch's issue.deployed: run <id> completed <t>`,
  `the deploy workflow's run: <repo> <workflow> run <id> attempt <n>, job <job name>: <step> succeeded at <t>`,
  `the Legion daemon's boot on build <sha>`, `Dispatch's start on build <sha>`,
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
