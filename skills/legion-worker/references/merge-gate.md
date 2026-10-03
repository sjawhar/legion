# The merge gate: review, retro, READY, and the production check

Part of `skill://legion-worker`. Read it when you are the reviewer submitting a review or an
approval, the implementer pushing the `.legion/` deletion or recording the production check, or
the merger publishing READY. Every path it cites is in sjawhar/legion.

The order, in full: the tester's evidence green → the implementer's `.legion/` deletion push →
the reviewer's approval of that head → retro → the merger's READY → the human merge → the
implementer's production check. After the approval, only retro's `docs/solutions/` commit leaves
it standing on its own (*Retro*, below). A conflict-forced merge goes back to the reviewer for a
confirmation or a new round, as the fingerprint decides (*The reviewer*, below, and
`skill://legion-worker/references/conflicts-and-rewrites.md`), and any other change voids it.

## The reviewer

- The reviewer verifies the `CI`, `Threads`, and `E2E` facts against GitHub directly —
  never from a handoff — then runs `task(agent="thermonuclear-deep-review")` and
  `task(agent="thermonuclear-code-quality")` once at that head — the head the implementer's
  simplify pass left final — and records the verdict.
  Approval is refused while either `E2E (implementer)` or `E2E (tester)` is missing: `REQUEST_CHANGES` naming the missing line.
  Skip the `Thermo` line entirely on a docs-only PR. Submit **one review per round** —
  `REQUEST_CHANGES` when any correctness finding stands, otherwise `COMMENT` while the head
  still carries `.legion/`; `APPROVE` only for a head that carries no `.legion/` — the head
  that differs from the reviewed one by the `.legion/` deletion alone, or, after a
  conflict-forced rebase, the new head whose fingerprint equals the approved head's — always
  named by SHA — carrying every inline comment in that single
  call: `legion gh -- api --method POST repos/{owner}/{repo}/pulls/{number}/reviews --input body.json`
  with `commit_id`, `event` (`REQUEST_CHANGES`, `COMMENT`, or `APPROVE`), `body` (with the
  Legion footer), and a `comments[]` array of `{path, line, side, body}`, one entry per
  finding — never one `pr review` call per finding (each submission fires a `pr-review` wake).
  Then return the issue to the architect; when clean, have the architect send the implementer
  back to push the `.legion/` deletion, then review **that** head and approve it by name. After a
  conflict-forced rebase, compute the fingerprint (*The unchanged-diff check* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`) at the
  `commit_id` of your last submitted review and at the new head. Equal and that review was
  `APPROVE`: submit one more `APPROVE` naming the new head by SHA, its body naming both SHAs
  and the fingerprint — a confirmation, not a round; no thermo pass, no thread pass. Equal and
  that review was `COMMENT` or `REQUEST_CHANGES`: continue that round against the new head;
  nothing restarts. Different: a new round — thermo again, one review.
- Answer every thread you opened, and every thread a bot opened that is none of Legion's role
  Apps, as `skill://legion-worker/references/review-threads.md` says; the same reference says
  when every thread is settled enough to approve.

A reviewer's phase ends with its completion, not with its review. A round that writes a handoff
takes this order: write, commit and push the handoff; submit the review of the head that push
made, by its SHA; then complete. An approval waits for the CI verdict to settle green at that head
before you submit it, since an approval stands only on green checks and GitHub can dismiss one
once the head moves, and a verdict that settles red there makes the round's decision a request for
changes naming the failing checks; a request for changes does not wait, since it stands whatever CI says and the
issue leaves reviewing with it. A review of a head the handoff push then replaces names a head
the pull request no longer has. A round that writes none (the final approval of the `.legion/`
deletion head) reviews the head as it is. The daemon moves the issue once both are in —
the decision GitHub reports and your completion, in either order — so a review posted without a
completion leaves the issue in reviewing until you finish.

## Retro

- **Retro's commit does not void the reviewer's approval.** After the reviewer approves the
  cleaned head, retro commits its learnings under `docs/solutions/` on top of it; that commit
  stays, the approval stands, and the tree goes to the merger — never back to the tester or
  reviewer. Anything else above the approved head does void it, and the merger tells the
  architect the head must return to review instead of publishing. A conflict-forced rebase
  after retro moves those documents with the branch; retro never re-runs.

## The merger

- The merger runs `legion threads resolve --pr <n> --repo <owner>/<repo>` (it acts as the same
  code-writing App as the implementer; resolving a thread changes no commit, so this run never
  invalidates the approval), does not publish while any `left open` line remains or the command
  exits 1 (report the thread to the architect instead), then proves that rule with two commands.
  First `cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && jj -R
  "$LEGION_WORKSPACE" diff --from <approved-sha> --to <tip-sha> --summary`, whose output is quoted
  in READY (an empty output is quoted as `no file changes above the approved head`); then the same
  with `'~docs/solutions'` appended, which must print nothing. *The READY packet*: the merger
  always posts `READY #<n> at <current sha> (approved at <approved sha>) for <KEY> (<pr url>)`
  (the shape `packages/daemon/internal/prompts/roles/merger.md` defines), then the PR body's
  `Outcome:` line and its `Not proven / risk:` value — every bullet under that label joined with
  `; ` on the one READY line, or `none` — quoted from the `## For the reviewer` block at that same
  head (or one line saying the body carries no brief — the packet still publishes), then the
  `--summary` output and the PR body's gate facts, as a `dispatch_message` on the issue. When the
  `Legion addressing` line names a merge queue, it also publishes the same packet there with
  `envoy_publish`; a 404 means the Dispatch message remains the durable notice and the merger
  stays idle. The READY packet names both the implementer's and tester's `E2E` lines; a missing
  one is reported to the architect instead of published. Legion never merges.

## After the human merge

- **After a human merges, the implementer verifies in production.**
  The architect sends the implementer back once the merge lands; the implementer watches the
  deploy slot that carries the merge to `production-apply` (or the equivalent publish step),
  drives the changed path in production through the user's own access path, and records the
  observation on the PR and the issue before the architect signs off. A staging pass is not
  this, since a staging gate does not run every resource production does. If the slot fails on
  the change, the implementer owns the fix and the next slot.
  The record has three places: the PR body's `Production:` line, one pull-request comment
  carrying the Legion footer, and a `dispatch_message` on the issue — the reviewer and merger
  read GitHub, the architect reads the issue. When the deploy that carries the merge has not
  happened (a shared profile still holding the previous plugin release, a daemon still running
  the previous commit, a slot nobody has run), open a `dispatch_ask` that starts with the
  production gap and why it matters, then names the required install or restart step, its risk,
  and outcome-named options. Keep the `Production:` line at `pending <what is missing>`, and
  complete the check once the human answers. Never record a staging pass as the production check,
  and never let the architect sign off on a `pending` line.
