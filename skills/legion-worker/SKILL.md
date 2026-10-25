---
name: legion-worker
description: Use when dispatched as a per-process Legion phase worker — planner, implementer, tester, reviewer, or merger — booted from the daemon's LEGION_* environment.
---

# Legion Phase Worker

If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong.

You work one phase of one issue in a jj workspace every role of the issue shares; the architect owns the tree. Your role prompt gives the workspace binding, `gh`, the push, the handoff file, `handoff_complete` and how long your session stays; this skill adds the rest.

## Every phase, in this order

1. **Read first.** Read the issue, then each committed handoff before your phase, with `read` from `$LEGION_WORKSPACE/.legion/<issue>/`, in order: `plan.json`, `implement.json`, `test.json`, `review.json`. Where one disagrees with your memory or a transcript, the file wins: it is the copy that survived. At each later assignment read them again, your own included, since other phases may have committed since. After a `Your workspace was recreated…` message, read `.legion/<issue>/workspace-recovered.json` too, once.
2. **Commit only your phase's work**, with `jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`, each commit holding only your phase's paths, so the next phase sees what you changed.
3. **Put back what you copied in.** Before you write the handoff, run `jj -R "$LEGION_WORKSPACE" status` and remove or restore every file you copied or fetched in that is not your phase's work, such as another branch's test rig. With jj the working copy is the commit: the next role's first commit builds on what you left, and its push ships it.
4. **Write the handoff** your role prompt names. Each `proof` entry, the implementer's or the tester's, is an object of six non-empty strings: `criterion` (the acceptance line it proves), `surface` (where a user reaches the change), `command`, `observed`, `headSha` (the commit it ran at) and `negativeControl` (a deliberately broken input and the refusal seen); `skill://legion-worker/references/pr-body.md#what-a-proof-is` defines each. To add to a handoff already on disk, `read` it, edit it, and `write` it whole with `completed` set to now.
5. **Push, then complete**, as the push section below says. `handoff_complete` is your only way to end a phase: add no label, keep no status of your own and run no loop of your own, since the daemon owns the issue's state and the architect its decomposition and gates.

## Stop rules

- **A refused completion is information, not a retry loop.** Quote the answer verbatim to the architect, so "my work is lost" and "my work belongs to an ended run" read differently. `HANDOFF_STALE_GENERATION` (the issue is on a newer run, or your turn began before the daemon recorded your task, which it then sends again) or `HANDOFF_NO_RUN` (your claim holds no task): push nothing further and tell the architect; when a task then arrives whose work is already committed, call `handoff_complete` again, and never redo the work or write a second handoff. `HANDOFF_NOT_CURRENT_PHASE`: the issue left your phase; report, never complete again. `HANDOFF_ALREADY_RECORDED`: the first answer (an acceptance, or any refusal but `HANDOFF_NO_RUN`) stands; if you did not see it, tell the architect. A reminder is not a task: after an accepted completion, a second call is refused.
- **After your phase completes, the workspace is read-only for you**: no commit, no `jj new`, no edit to a tracked file, in any later turn until a new task names your phase. The active phase owns it, and you stay live only to answer questions.
- **Never start another Legion role**: the daemon starts every phase worker.
- **Never end your turn blocked on a decision someone else owns.** Send a product, scope, design, lifecycle or cross-phase decision to the owning architect as your role prompt says, with the issue, phase, verified observation, what you tried and the decision needed; a human-only to-do is a `dispatch ask`. Otherwise proceed: a phase that stops silently holds its issue until someone notices.
- **No surface reaches the changed path** is a report to the architect, never a reason to complete (`skill://legion-worker/references/pr-body.md#when-no-surface-reaches-the-changed-path`).
- **Asking another role**: if its topic answers 404, read its committed handoff instead.

## References

Read each by its `skill://` link: a read by path stops at 300 lines.

| When | Read |
| --- | --- |
| You write or edit the PR body, record or verify a proof, or run the simplify pass | `skill://legion-worker/references/pr-body.md` |
| Before a push, to check your commits' App identity | `skill://legion-worker/references/default-branch-check.md` |
| You reply on, list or resolve review threads, or judge a bot's finding | `skill://legion-worker/references/review-threads.md` |
| A conflict, a retarget, comparing heads after a conflict merge, or rewriting a pushed commit | `skill://legion-worker/references/conflicts-and-rewrites.md` |
| You write the `skip-checks: true` trailer, or expect a body edit to re-run a check | `skill://legion-worker/references/skip-checks-trailer.md` |
| You review or approve, build READY, or run the production check | `skill://legion-worker/references/merge-gate.md` |
| The issue renames a repository, package, or URL across the codebase | `skill://legion-worker/references/systematic-rename.md` |
| The issue deletes code, a command, or documentation | `skill://legion-worker/references/cleanup-deletion.md` |
| You need a tool the image may not carry, before you install anything | `skill://legion-worker/references/tools.md` |
| A problem outside your issue, or a failed production check | `skill://legion-worker/references/findings.md` |

## Every role pushes its own commits

- Before a push, read `jj -R "$LEGION_WORKSPACE" log -r 'ancestors(@, 5)'` (earlier phases' commits in it are expected) and run the identity check (`skill://legion-worker/references/default-branch-check.md`). Add no attribution trailer: the pane's jj adds `Omp-Session:`.
- A push whose commits touch only `.legion/` ends its head's message with `skip-checks: true`, unless it is retro's push (the implementer's retro phase, before the merger's). Retro's push never carries the line, whatever paths it touches, because it is the last push before the merge and a base that requires a check will not merge a head whose checks never ran. No other push carries it. The daemon classifies a push by its paths, never by the trailer, so a wrong trailer can stall the issue but never makes it pass.
- A planner whose issue moved back from implementing starts on the remote tip, `jj -R "$LEGION_WORKSPACE" new legion/<KEY>@origin`, so its handoff never sits on the implementer's unpushed commits.

## PR body, review, and the merge gate

- The implementer opens the pull request with the line `Dispatch: <KEY>` in its body, the daemon's link to the issue when the branch name is ambiguous. Later phases push to that branch and edit that pull request, never a new one.
- **No deferrals.** A finding that changes behaviour, hides an error, or breaks a gate is fixed in this pull request. Only cleanup that changes no behaviour (naming, duplication, wording) may wait, on the body's one `Fast-follow:` line.
- **The merge gate**, in order: the tester's evidence green → the reviewer's approval of the head → the architect's acceptance → retro → the merger's READY → the merge, which closes the issue → the implementer's production check, after the close. When the daemon accepts READY, it submits the merge of READY's head, unless the answer's note begins `MANUAL_MERGE_REQUIRED:`, where a person merges it; no role runs `gh pr merge`.
- Append `<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->` to every pull-request comment and review you post: the daemon finds the session behind a review by it.
- Never write a GitHub issue, by `gh issue` or a non-GET `gh api` on `/issues` (post or edit a PR conversation comment with `gh pr comment`): Legion's issues live on Dispatch. Never run `gh config set` or `gh alias set`, and leave `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` empty: the daemon rewrites your gh directory in place, and a set variable outranks it.
- Before you report a resource unreachable, check `AGENT_SECRETS_URL` and `AGENT_SECRETS_KEY_DIR` (then `agent-secrets <SECRET> -- <command>`; sjawhar/legion `docs/kubernetes.md`, "Operator configuration") and `AWS_CONFIG_FILE` (read its profiles): a deployment can give your pod either. `agent-secrets` refuses a secret its grant lacks and names it; that refusal is final. Your `task` subagents and background jobs inherit both, and your `gh` and `git`.

## Deployment instructions

Deployment instructions in your prompt override this skill where they conflict, except four rules: no deferrals; bringing the base into the branch only on a real conflict or a retarget (`skill://legion-worker/references/conflicts-and-rewrites.md#reintegrating-the-base`); the implementer's own proof on a production-like surface at the head that merges, an applied simplify head included (`skill://legion-worker/references/pr-body.md#what-a-proof-is`, `skill://legion-worker/references/pr-body.md#the-rules-every-phases-evidence-follows`); and the implementer's production check after the close (`skill://legion-worker/references/merge-gate.md#the-production-check-after-the-close`).
