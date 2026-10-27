# Legion Tester

## Test mechanics

Then read the plan handoff's `requiredSkills` for your role and follow those too.

Record the exact command or run id, what you observed, the head SHA, and one negative control in the PR body's `E2E (tester)` line. Record the verdict on the implementer's proof in your `test` handoff as `implementerProof` (`{verdict: "verified" | "rejected", how}`). A test handoff whose predecessor carried no proof is a test failure, not a gap for the tester to fill: record it in `failures`, set `implementerProof.verdict: "rejected"`, and complete the phase as a failure; the daemon sends the issue back to the implementer. Otherwise, add your own proof before completing: the `E2E (tester)` line in the PR body and the `proof` array in your handoff, required whenever you report no failure, each entry with the fields `skill://legion-worker` names where it writes a handoff. Nothing checks the handoff's shape at completion: the reviewer reads what you wrote.

Environment or secret-scrub evidence (for example "`LEGION_*`/`DISPATCH_*`/`ENVOY_*` unset") is recorded once, in your `test` handoff, and only when the issue's acceptance criteria call for it — never re-pasted into the PR body on every round. Read the plan and the implementation handoff with `dispatch handoff-read --issue <issue> --phase plan` and `--phase implement` (your task's `Handoff:` line names the implementer's version), and an earlier round's with `--phase test` or `--phase review`; choose checks that prove the observable contract. You may use ordinary oracle, scout, or reviewer subagents, but never spawn a Legion role.

## Rebases

For the unchanged-diff fingerprint procedure, follow `skill://legion-worker/references/conflicts-and-rewrites.md`. Update the `E2E` head SHA with `rebase re-check <old-sha> → <new-sha>: fingerprint unchanged, bare gates only` when the fingerprint is equal.

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

Every testing round records a `test` handoff on the Dispatch issue, a rebase re-check included, as the daemon part below says: a JSON object with `schemaVersion: 1`, `phase: "test"`, `issue: "<issue>"`, `completed: "<RFC 3339 UTC time of writing>"`, `implementerProof`, `failures` (each `{criterion, evidence}`) with `failed` counting them, and `proof` whenever you report no failure; nothing stamps the first four for you. Write it with the `write` tool to `$TMPDIR/<issue>-test.json`, record it with `dispatch handoff-write --issue <issue> --phase test --body-file "$TMPDIR/<issue>-test.json"`, then report completion.
