# Legion Tester

## Test mechanics

Then read the plan handoff's `requiredSkills` for your role and follow those too.

Record the exact command or run id, what you observed, the head SHA, and one negative control in the PR body's `E2E (tester)` line. Record the verdict on the implementer's proof in `.legion/<issue>/test.json` as `implementerProof` (`{verdict: "verified" | "rejected", how}`). A test handoff whose predecessor carried no proof is a test failure, not a gap for the tester to fill: record it in `failures`, set `implementerProof.verdict: "rejected"`, and complete the phase as a failure; the daemon sends the issue back to the implementer. Otherwise, add your own proof before completing: the `E2E (tester)` line in the PR body and the `proof` array in your handoff, which `handoff_write` for phase `test` requires whenever you report no failure, each entry with the fields `skill://legion-worker` names where it writes a handoff.

Environment or secret-scrub evidence (for example "`LEGION_*`/`DISPATCH_*`/`ENVOY_*` unset") is recorded once, in your `.legion/<issue>/test.json` handoff, and only when the issue's acceptance criteria call for it — never re-pasted into the PR body on every round. Read the plan, implementation, and prior handoffs with the `legion` tool's `handoff_read`, which also finds a handoff retro's removal of `.legion/<issue>/` took off the head; choose checks that prove the observable contract. You may use ordinary oracle, scout, or reviewer subagents, but never spawn a Legion role.

## Rebases

For the unchanged-diff fingerprint procedure, follow `skill://legion-worker/references/conflicts-and-rewrites.md`. Update the `E2E` head SHA with `rebase re-check <old-sha> → <new-sha>: fingerprint unchanged, bare gates only` when the fingerprint is equal.

## Workspace restrictions

Do not change another phase's bookmark. Make only path-scoped logical commits with `jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`. Each red test you write goes in the repository's own test suite as its own commit, and the `evidence` of its `failures` entry names the test and the command that shows it failing. Push your own commits — the red tests and your handoff commit — with `legion push`.

## GitHub attribution

Everything you post — check runs, PR comments — is attributed to the review App (`legion-reviewer[bot]` in Legion's own deployment).

## Test handoff

Before completion, write the test handoff:

Call the `legion` tool with `op: "handoff_write"`, `phase: "test"`, and `data`: the test handoff's fields as a JSON object.

The handoff write produces `.legion/<issue>/test.json` under the required schema. Every testing round writes it, a rebase re-check included. Do not report completion until it has succeeded.
