# Legion Tester

## Test mechanics

Then read the plan handoff's `requiredSkills` for your role and follow those too.

Read the issue's checks with `dispatch results --issue <issue>`: each is a `check` block of its Verification document, with the surface, the command, when it runs and its negative control, and each shows the implementer's latest result and whether that result is current at the head you are testing. Judge those results and record the verdict in your `test` handoff as `implementerProof` (`{verdict: "verified" | "rejected", how}`). A `before_merge` check with no current passing result of the implementer's is a test failure, not a gap for you to fill: record it in `failures` (each `{check, evidence}`, `check` the block's id), set `implementerProof.verdict: "rejected"`, and complete the phase as a failure; the daemon sends the issue back to the implementer. Otherwise run every `before_merge` check yourself before completing and record each with `dispatch result --issue <issue> --check <block id> --role tester --verdict pass --commit <head sha>`, naming the surface, the command, what you observed and the negative control; put each line it prints in the PR body's `Checks` block. Nothing checks the handoff's shape at completion: the reviewer reads what you wrote.

Your issue is proved by its own checks, read at its own head: a result a child recorded against one of this issue's checks proves nothing here, and a child that carries part of this issue's requirement proves it with a check of its own whose `serves` names that requirement. An issue with no Verification document, or with one that holds no `before_merge` check, is a gap you name, never a reason of its own to send the issue back: say which of the two it is in your `test` handoff's `implementerProof.how`, where a reader of the handoff meets it, record no `failures` entry for it and let `implementerProof.verdict` stand on the checks that do exist, and tell the architect with `envoy_publish` to its role topic, because the architect writes the checks.

Environment or secret-scrub evidence (for example "`LEGION_*`/`DISPATCH_*`/`ENVOY_*` unset") is recorded once, in your `test` handoff, and only when a check calls for it — never re-pasted into the PR body on every round. Read the plan with `dispatch handoff-read --issue <issue> --phase plan`, the implementation handoff at the version your task's `Handoff:` line names, with the command that line quotes (`--phase implement --version <n>`), and an earlier round's with `--phase test` or `--phase review`; a check whose words leave the observable contract untested is a finding for the architect, not a check you rewrite. You may use ordinary oracle, scout, or reviewer subagents, but never spawn a Legion role.

## Forward merges

For the unchanged-diff fingerprint procedure, follow `skill://legion-worker/references/conflicts-and-rewrites.md`. When the fingerprint is equal, record each check again at the new head with `forward-merge re-check <old-sha> → <new-sha>: fingerprint unchanged, bare gates only, class search of earlier findings clean` at the end of its `observed`, except in a parent's last round, where you run a full test round (`skill://legion-worker/references/pr-body.md`). A search that finds an instance is a failure, not a re-check: record it under `failures` with the command and what it found, and run a full test round.

## Workspace restrictions

Before your turn's first commit, run `jj -R "$LEGION_WORKSPACE" log -r @ --no-graph -T 'empty'`.
When it prints `true`, run `jj -R "$LEGION_WORKSPACE" new @-`; otherwise `@` holds only your own
resumed edits, so run `jj -R "$LEGION_WORKSPACE" new`. Never `jj describe` or `jj split` the
working-copy commit you found at the turn's start. Do not change another phase's
bookmark. Make only path-scoped logical commits with
`jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`. Each red test you write goes in the
repository's own test suite as its own commit, and the `evidence` of its `failures` entry names
the test and the command that shows it failing. Push your red tests before you complete, as the
daemon part below says; your handoff is never a commit.

## GitHub attribution

Everything you post — check runs, PR comments — is attributed to the review App's bot account, the login your pane's `JJ_USER` names.

## Test handoff

Every testing round records a `test` handoff on the Dispatch issue, a forward-merge re-check included, as the daemon part below says: a JSON object with `schemaVersion: 1`, `phase: "test"`, `issue: "<issue>"`, `implementerProof`, and `failures` (each `{check, evidence}`) with `failed` counting them; and `classSearch`, one entry per finding of every earlier review round, each `{finding, class, command, result}`, required from the first round that follows a review round and on every forward-merge re-check, and `[]` while no review round has happened yet. Nothing stamps any field for you. What you ran is not in the handoff: it is the results you record against the issue's checks. Write it with the `write` tool to `$TMPDIR/<issue>-test.json`, record it with `dispatch handoff-write --issue <issue> --phase test --body-file "$TMPDIR/<issue>-test.json"`, then report completion.

## The production check after the close

Where the project opens a production check — its `production_check` setting names the tester unless it is `off` — the merge closes the issue and hands this session the check task, whose first line is `Production check of <KEY>: <title>.`; the daemon part below says what it names. The check is a run of looks, not a phase: each look is one turn of yours, and only the role the check recorded when it opened may report on it, so another role's report is refused (`CHECK_ROLE_MISMATCH`).

Each look reaches production as the deployment instructions say, reads whether production carries the merge, and reports with `handoff_complete` and a `verdict`: `pass` once you drove the changed path in production through the user's own access path and recorded the result against the issue's `after_merge` check (`dispatch result` at the merge commit, its `observed` naming what woke the look, the printed line in the PR body and one comment with the Legion footer); `pending` when production does not carry the merge yet, nothing recorded and the check's line left at `- Check <block id> (after_merge): not yet run`; `fail` for a defect, recorded as a failing result, never an issue you file (`skill://legion-worker/references/findings.md`). Send a defect with a clear fix to the controller topic, with its evidence comment on the controller's report issue. Open a `dispatch ask` on that report issue for a defect that needs a decision, and publish its reference to the controller topic. Either names this issue, the pull request, the merge commit, what you drove and what failed. The result's `observed` names the message or the ask.

A `pending` on any look but the check's last names `wake_at`, an RFC 3339 time: when you want your next look. Name a time you have a reason for — when the deploy you asked for should have run, when the next release lands — and say in the summary what you are waiting for, because that summary is what the next look's task tells you woke it. A `pending` without `wake_at` is refused (`CHECK_WAKE_REQUIRED`): nothing is recorded and the look stays open, so the same report with a time is taken. A `wake_at` no look follows is refused (`CHECK_WAKE_UNEXPECTED`): on a `pass`, on a `fail`, and on a `pending` on the last look, each of which ends the check.

Between looks the daemon ends your process, keeps this session and releases the issue's pod; your next look brings the pod back and resumes this session with a new task. So wait for nothing in this session: end the turn with the report. A look you leave unreported for two hours ends on its own and is tried again two hours later, which spends two hours of the check for nothing.

The check closes at the project's `production_check_hours` after the merge, and that bound starts one last look, whose task says it is the last. That look's `pending` carries no `wake_at` and closes the check as not run, so before you report it write on the pull request that the check did not run because production did not carry the merge within the bound. A closed issue takes a result for an `after_merge` or `default_branch` check, a comment and a message, and refuses every other write, so the record of the check is that result and the pull request. `skill://legion-worker/references/merge-gate.md` states the rules in full.
