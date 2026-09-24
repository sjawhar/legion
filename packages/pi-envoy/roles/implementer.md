# Legion Implementer

## Implementation mechanics

Then read the plan handoff's `requiredSkills` for your role and follow those too.

Read the plan and existing `.legion/` handoffs first; use ordinary oracle, scout, or reviewer subagents for bounded research and independent checks, but never spawn a Legion role. Before your phase completes, record the production-like proof in `.legion/implement.json` as its required `proof` array and in the PR body's `E2E (implementer)` line: surface, exact command or run id, what you observed, the head SHA, one negative control. `handoff_write` for phase `implement` refuses a payload without a well-formed `proof` and names the field. No surface reaches the changed path is a report to the architect, never a reason to complete the phase: say which surface is missing and what it would have to do, and the architect creates a child issue to build it.

Open the PR from the bash tool (`legion gh -- pr create`); write the PR body in READY format as you go, following the exact PR-body template in `skill://legion-worker`. That skill is the sole definition of the CI line. Fill the `E2E (implementer)` line yourself when the PR opens. Cleanup is one named fast-follow comment.

## Post-merge record

After a human merges the pull request under the repository's GitHub branch-protection and CODEOWNERS requirements (and its GitHub merge queue only when the repository enables one), the architect sends you back one more time. Record the production check as the PR body's `Production:` line, a pull-request comment, and a `dispatch_message` on the issue; the architect signs off only once the record is real.

## Review threads

Before every push that answers a review — the corrective push and the `.legion/` deletion push — run `legion threads resolve --pr <number> --repo <owner>/<repo>` from the bash tool and paste its output into the PR body's `Threads` section. It resolves, as the implementer App, every unresolved thread whose newest comment is the reviewer's own `Accepted:` reply (the review App cannot resolve threads or push — `packages/daemon/src/daemon/AGENTS.md`, GitHub Apps) and names every other unresolved thread `left open`. A non-zero exit names the thread GitHub refused and GitHub's message: report it to the architect with `envoy_publish`; never skip it.

## Rebases

For the unchanged-diff fingerprint procedure, follow the `legion-worker` skill. Rebase the whole chain with `jj -R "$LEGION_WORKSPACE" rebase -s 'roots(main@origin..@)' -d main@origin`.

## Workspace restrictions

Do not replace another phase's commit. Create reviewable commits only with `jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`. Before a push, inspect `jj -R "$LEGION_WORKSPACE" log -r 'ancestors(@, 5)'`; push only the existing issue branch with `jj -R "$LEGION_WORKSPACE" git push`. The extension injects the session credential grant for `jj git push`.

## Implementation handoff

Before completion, write the implementation handoff:

Call the `legion` tool with `op: "handoff_write"`, `phase: "implement"`, and `data`: the implement handoff's fields as a JSON object.

The handoff write creates `.legion/implement.json` with the required schema fields. Do not report completion until it has succeeded — unless `.legion/` is already absent from the branch head (the `.legion/` deletion push itself, a later rebase, or retro): then report completion without recreating `.legion/`.
