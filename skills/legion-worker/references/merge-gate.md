# The merge gate: review, retro, READY, and the production check

Part of `skill://legion-worker`. Read it when you are the reviewer submitting a review or an
approval, the implementer recording the production check, or the merger building READY. Every
path it cites is in sjawhar/legion.

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
  `gh pr view <n> --json baseRefName,headRefOid,headRefName`, four reads. The required set is the
  base branch's rulesets' required checks and its branch protection's, when it has any. Both
  branch endpoints take the base as a URL path, each segment escaped and each `/` kept (a raw `#`
  or `%` reads another branch), built in the same command from the name GitHub returns, never
  pasted or carried over from an earlier command:

  ```bash
  base=$(gh pr view <n> -R <o>/<r> --json baseRefName -q .baseRefName) && \
    base_path=$(jq -rn --arg b "$base" '$b | split("/") | map(@uri) | join("/")') && \
    echo rulesets: && gh api "repos/<o>/<r>/rules/branches/$base_path" --jq '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context' && \
    echo protection: && gh api "repos/<o>/<r>/branches/$base_path" --jq '.protection.required_status_checks.checks[].context'
  ```

  A read GitHub refuses stops the command there, and is reported to the architect, never guessed. The CI head, the head of the
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
  nothing restarts. Different, or either run printed no fingerprint: a new round — thermo again,
  one review.
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
  A required check or workflow still running refuses nothing; a failed or missing one does. A
  refusal records nothing; the merger reports it to the architect unless it says to complete
  again, and never pushes a commit to make CI run. On success the daemon posts the packet as a
  `dispatch message` on the issue, publishes it to the project's merge queue role when one is set,
  and says on the issue when that role has no live holder; the answer's `note` says when READY was
  published on a pull request a person already merged, whose head can no longer change, or on a
  base that requires no check, which the packet itself also says, or carries the by-hand note of a
  pull request a person merges.
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
  row waited before, does the row end and tell the architect once (`merge-stalled`). A reopen at
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
