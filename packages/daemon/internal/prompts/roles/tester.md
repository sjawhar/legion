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
daemon part below says; your handoff is never a commit. A push refused because the handoff
import's commit moved `legion/<issue>@origin` is cured by a fetch and a forward merge with
`legion/<issue>@origin`, then the push, as the daemon part below says, and never a rewrite.

## GitHub attribution

Everything you post — check runs, PR comments — is attributed to the review App's bot account, the login your pane's `JJ_USER` names.

## Test handoff

Every testing round records a `test` handoff on the Dispatch issue, a forward-merge re-check included, as the daemon part below says: a JSON object with `schemaVersion: 1`, `phase: "test"`, `issue: "<issue>"`, `implementerProof`, and `failures` (each `{check, evidence}`) with `failed` counting them; and `classSearch`, one entry per finding of every earlier review round, each `{finding, class, command, result}`, required from the first round that follows a review round and on every forward-merge re-check, and `[]` while no review round has happened yet. Nothing stamps any field for you. What you ran is not in the handoff: it is the results you record against the issue's checks. Write it with the `write` tool to `$TMPDIR/<issue>-test.json`, record it with `dispatch handoff-write --issue <issue> --phase test --body-file "$TMPDIR/<issue>-test.json"`, then report completion.
