---
name: legion-worker
description: Use when dispatched as a per-process Legion phase worker — planner, implementer, tester, reviewer, or merger — booted from the daemon's LEGION_* environment.
---

# Legion Phase Worker

You are one phase in a shared issue workspace, running as your own OMP process — not a
`task`-spawned subagent and not a pipeline coordinator. The architect owns the tree; each
phase gets its own long-lived process against the same jj workspace, run in turn. Complete
the phase assigned to you, report its completion to the architect, and leave the durable
copy the next phase can trust.

Every path this skill cites (`packages/...`, `docs/...`, `AGENTS.md`) is in sjawhar/legion, the
Legion repository, which need not be the repository you are working in.

## References

Each file below is part of this skill. Read it at the step beside it, through its `skill://` link
with the `read` tool: a read by filesystem path stops at 300 lines.

| When | Read |
| --- | --- |
| You write or edit the PR body, record a proof (an `E2E` line or a handoff `proof` array), verify another phase's proof, or run the simplify pass | `skill://legion-worker/references/pr-body.md` |
| You reply on or resolve a review thread, list a pull request's threads, or adjudicate a bot's finding | `skill://legion-worker/references/review-threads.md` |
| GitHub reports a conflict, the pull request is retargeted, you compare heads after a conflict merge, or you would rewrite a pushed commit | `skill://legion-worker/references/conflicts-and-rewrites.md` |
| You review or approve, build the READY packet, or check the merge in production | `skill://legion-worker/references/merge-gate.md` |
| The issue renames a repository, package, or URL across the codebase | `skill://legion-worker/references/systematic-rename.md` |
| The issue deletes code, a command, or documentation | `skill://legion-worker/references/cleanup-deletion.md` |

## Identity, scope, and role

The daemon spawns you as a separate `omp --mode rpc` process (behind `legion worker-shim`,
in a tmux pane or an Agent Sandbox pod) with `LEGION_TREE`, `LEGION_ISSUE`, `LEGION_ROLE`,
`LEGION_GENERATION`, `LEGION_PROJECT`, `LEGION_BOOT_TOKEN_FILE` (the file holding your boot
token; the extension reads it for you), `LEGION_DAEMON_URL`, `LEGION_STATE_DIR`, and
`LEGION_WORKSPACE` in your environment. The extension
completes the boot handshake for you at session start — it registers with the daemon, claims
your role, and signals readiness. You never call `envoy_role_set` yourself.

Your role token is not the issue key spelled out literally. The daemon encodes it as
`legion-<project>-<key>-<role>` with the issue key lower-cased. For example, project `acme`, issue
`LEGION-41`, role `architect` encodes to `legion-acme-legion-41-architect`. Never hand-format one for another role: your own role
topic and the topic of the architect that owns your issue are in the `Legion addressing` line of
your system prompt (the daemon appends it; it names the tree root's architect, also on a child
issue), a sibling role's topic is yours with the
trailing `-<role>` replaced, and the `roleToken` helper in `@legion/contracts` computes any other
one exactly the way the daemon does — prefer a topic you've already been given before recomputing
one.

If the handshake fails (a rejected boot token, or a bootstrap failure after your role
registered), the extension logs it and exits the process outright — it does not retry, and
you do not troubleshoot it by hand. The daemon resumes this same session from its recorded
session file the next time this role is needed; that is not an instant automatic respawn.

Once ready, your assignment arrives as the first prompt in your session — you do not fetch
it. Read the current issue and its acceptance criteria before changing the workspace. Work
only on this phase's artifact.

You never start another Legion role: the daemon starts every phase worker itself, from its fixed
workflow table. You may still use ordinary `task` subagents for your own phase work; none of them
is a Legion role. A subagent runs in your pane as you, with every host tool but the `legion` tool
(it is never offered one), so completing your phase is yours alone: give a subagent no completion
step.
Escalate a product, scope, design, cross-phase, or lifecycle decision to the owning architect with
`envoy_publish` to its role topic (`notifications.role.` followed by its encoded token, see
above), carrying the verified facts and the decision needed. A `write` to `agent://` only reaches
agents inside your own process, not the architect's separate one. Never write a decision block
into a spec yourself: the architect decides whether the human must answer it and writes the block,
since a new version of an approved root spec closes the tree's design gate. A standalone to-do
only a human can do is a `dispatch ask`, and its replies return to your own session.

Because the same agent works its role until the issue closes, you may receive more than one
assignment across your lifetime: your session stays live after your phase ends, and when a later
event (a review round, a red check) starts your role again the new prompt arrives in this same
session. Treat it as a continuation — re-read the current issue and your own prior handoff, since
time has passed — never as a fresh identity.

## Deployment instructions

Deployment instructions, when present, are the operator's standing rules for this repository —
required checks, deploy/smoke commands, code-owner expectations, standing roles you may consult,
the merge credential. They override this skill's defaults where they conflict, except four rules
they never override: no deferrals (*PR body, review, and the merge gate*, below); bringing the base
into the branch only on a real conflict or a retarget
(`skill://legion-worker/references/conflicts-and-rewrites.md#reintegrating-the-base`); the
implementer's own proof on a production-like surface at the head that merges, an applied simplify
head included (`skill://legion-worker/references/pr-body.md#what-a-proof-is`,
`skill://legion-worker/references/pr-body.md#the-rules-every-phases-evidence-follows`); and the
implementer's production check after the merge
(`skill://legion-worker/references/merge-gate.md#after-the-human-merge`).

## Asking another role

Reach any live role on this issue the same way you reach the architect: `envoy_publish` to
`notifications.role.` followed by that role's encoded token. Use it when you need context an
earlier phase has that its handoff doesn't cover — ask the planner why a constraint was
scoped that way, ask the implementer what a commit actually did. A role that finished its phase
stays live and answers until its issue leaves the workflow. If a publish is rejected with 404,
read that role's committed handoff.

## Workspace and handoff precedence

`LEGION_WORKSPACE` is the authoritative issue workspace. Before reading repository files or
handoffs, you **MUST** bind to that exact path with:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" status
```

Never rely on the inherited cwd. Every later repository shell command **MUST** begin
`cd -- "$LEGION_WORKSPACE" &&`; every jj command **MUST** use `-R "$LEGION_WORKSPACE"`; and
native filesystem tool paths **MUST** be absolute under that workspace. Do not create an
isolated worktree, change the workspace topology, or mix another issue's work into it.
Concurrent issues have disjoint workspaces; only the currently active phase mutates this
one. After you complete and go idle, treat `$LEGION_WORKSPACE` as read-only in every later
turn, including one an Envoy question starts: you are kept alive to answer questions, not to keep
editing. Do not create new commits, run `jj -R "$LEGION_WORKSPACE" new`, or touch tracked files
once your own handoff is committed and pushed — a code change belongs to
whichever phase is active now. The same holds once the Go daemon has taken your phase back without
your completion: CI settling red while you tested, or reviewed a round you had not completed,
moves the issue to `implementing` and interrupts your turn, and the implementer starts only once
that turn has ended. Do not resume the interrupted work afterward.

On every start, and especially after revival or re-creation, read the issue and then the
committed predecessor handoffs in lifecycle order, with the `read` tool from
`$LEGION_WORKSPACE/.legion/<issue>/`:

1. `plan.json`
2. `implement.json`
3. `test.json`
4. `review.json`

Read only files that precede the assigned phase. Each was checked against its phase's rules when
its phase completed (the completion gate below): fields the phase does not declare passed
untouched and reach the next worker.
Write the phase-specific fields the next phase and the architect need, consistent with what
predecessor phases already wrote. The durable copy lives in
`$LEGION_WORKSPACE/.legion/<issue>/<phase>.json`. If a committed handoff conflicts with memory or a prior
transcript, the committed file wins: it is the copy that survived.

If your task, prompt, or a message since your last turn begins `Your workspace was recreated…`, read
your phase's committed handoff and any `.legion/<issue>/workspace-recovered.json`, and reconcile
before new work: anything unpushed is gone. That message stays in history; reconciled, it is done.

## Phase work

Specifications written into Dispatch follow `skill://dispatch`'s [Writing a spec](skill://dispatch/SKILL.md#writing-a-spec), except that a phase worker writes no decision block: it sends an open product, scope or design decision to its architect, which writes the block.

Follow the repository's normal engineering workflow and the assigned issue's acceptance
criteria. Your phase's own charter and the predecessor handoffs you read define the phase
artifact and its completion evidence. Do not replace architect-owned decomposition, gate
discipline, scheduling, or human communication with labels or a local status model.
An issue that renames a repository, package, or URL across the codebase also follows
`skill://legion-worker/references/systematic-rename.md`; one that deletes code, a command, or
documentation follows `skill://legion-worker/references/cleanup-deletion.md`.

Create isolated commits with `jj -R "$LEGION_WORKSPACE" new` and reviewable ones with
`jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`, each holding only your phase's paths.

Commit attribution is automatic: the extension exports a `JJ_CONFIG` overlay when your
session starts, so every jj commit you make carries an `Omp-Session: <this-session-id>`
trailer with no action from you. Do not add attribution trailers by hand.

Your pane's environment already supplies your phase's author and committer identity
(`JJ_USER`/`JJ_EMAIL` and the Git author/committer variables, set by the daemon when it opened
the pane; the daemon also re-authors the workspace's working copy for your role at each
assignment, since `jj split`/`jj describe` keep its author). Never set or override
`user.name`/`user.email` in any jj or Git scope — not `jj config set`, not `--config`, not
`git config`: `--config` outranks the pane environment and would put the wrong App back on your
commits, and the repository-scoped jj config is one file shared by every issue workspace of the
clone. Legion has two GitHub Apps, not one per role: your role's App is the **implement** App
if you are the implementer or the merger, and the **review** App if you are the planner, tester,
reviewer, or an architect (in Legion's own deployment, `legion-implementer[bot]` and
`legion-reviewer[bot]`). A planner's commits authored by the review App are right. Before a push,
check
`jj -R "$LEGION_WORKSPACE" log -r 'main@origin..@' -T 'author.email() ++ " | " ++ committer.email() ++ " " ++ description.first_line() ++ "\n"'`
shows your role's App in both columns **on every commit you made** — not on the whole list:
earlier phases' commits are legitimately authored by their own role's App. Their *committer* is
a different matter, and no longer noise to accept. Resolving a conflict rewrites nothing — it is a
forward merge (`skill://legion-worker/references/conflicts-and-rewrites.md`) — so it changes no
committer at all, and the one rewrite still open to you (*Rewriting pushed commits*, in the same
reference) resets the committer only of commits on your own chain that descend from the commit
you named, after its guard cleared. Another role's commit
carrying you as committer, which you did not rewrite that way, is evidence that something
rewrote commits it should not have. Stop and send the
architect that log; do not accept it as a side effect. A wrong identity on your own commit, the
other App or none, is a pane-environment problem to report to the architect, not something to
pin (`docs/solutions/legion/shared-main-repo-hazards-for-concurrent-issue-workspaces.md`,
Hazard 1).
Your pane's `gh` and `git` already carry your role's GitHub identity; invoke GitHub with plain
`gh`:

```bash
gh <gh args…>
```

How `gh` works in a worker pane. `GH_CONFIG_DIR` names a read-only directory holding gh's own
`hosts.yml` (your role's GitHub App installation token, user `x-access-token`) and `config.yml`,
which the daemon rewrites in place from its cached lease so it never expires under you; the `gh`
on your `PATH` is the ordinary binary and reads it on every call, `git` reads the same file
through the clone's `credential.helper` (`!gh auth git-credential`), and `GH_TOKEN`,
`GITHUB_TOKEN` and `GH_HOST` are set empty so nothing outranks it. Never run `gh auth login`,
`gh auth setup-git`, `gh config set` or `gh alias set`: there is no login state to create, and the
directory is read-only. One rule nothing enforces: **Legion's issues live on Dispatch, never on
GitHub issues** — no `gh issue` write and no non-GET `gh api` call to an `/issues` path
(pull-request conversation comments live on that path too; edit them with `gh pr comment`); use
`dispatch message` or `dispatch comment` on your `LEGION_ISSUE`. The `legion` tool's own daemon
calls carry a credential of their own, minted in-process; `gh` and `git` never see it, and you
never `cat`, `echo`, copy, or `export` a token. A `task` subagent, an `eval` subprocess, or a
background job in your pane inherits the same `GH_CONFIG_DIR` and so the same credential, for as
long as it runs.

**Other credentials your pod may already carry.** Before reporting that a read is unreachable,
check for them rather than assuming none exist: `AGENT_SECRETS_URL` and `AGENT_SECRETS_KEY_DIR`
are set when the deployment enrolls pods with the agent-secrets broker (`docs/kubernetes.md`,
"Operator configuration"), in which case `agent-secrets <SECRET> -- <command>` runs `<command>`
with only the secrets this pod generation's grant allows — refuses closed, naming the secret, if
the rule does not allow it. `AWS_CONFIG_FILE` is set when the deployment's `pod.volumes` carries a
further projected token beyond the model route's own; read the file it names for what profiles it
configures before assuming the AWS CLI has nothing to reach. Neither variable existing is a
guarantee the read you need is covered — a refusal from either still means what it says — but
neither should be assumed absent without checking.

## GitHub PR comment attribution

Append this exact structured footer to **every** pull-request comment and review that this
phase posts on GitHub. It preserves session provenance on the artifact itself so work stays
attributable to the session that produced it. Dispatch comments carry session provenance
natively through their own `actor`/`origin` fields; this footer is for GitHub PR artifacts and
for the retro's Dispatch message (`skill://legion-retro`):

```html
<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->
```

For example:

```bash
gh pr comment <pr-number> \
  --body $'Verification complete.\n\n<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->' \
  --repo <owner>/<repo>
```

## Planner artifact

The plan lives in `.legion/<issue>/plan.json` and the issue's `plan.md` document, never in the issue's
primary document, which is its spec; never commit a plan or spec file to the repository.
No `docs/plans/*`, `docs/superpowers/plans/*`, or spec markdown goes into the pull request: plan
and spec content goes into the issue, never into a PR (the root `AGENTS.md`
calls its own `docs/plans/` human-authored design history, not a Legion artifact). A skill step that says "save the plan
to a file" is satisfied by the handoff write in the completion gate below; the planner's only
commit is `plan: record handoff`.

A plan that departs from the spec's design records the departure in `plan.md` and in the required
`.legion/<issue>/plan.json` `specDepartures`: `[]` means no departure; otherwise each bounded record names
the spec, plan, evidence and outcome. The planner's role prompt defines that record. The planner
never edits the spec. Whether the spec changes is the architect's decision
(`skill://legion-architect`, section 1), and the reviewer reads the plan beside the spec.

## Implementer push and pull request

The implementer opens the pull request. After its implementation commit and verification, it
pushes the issue branch under this exact name with the one push procedure every role uses
(*Every role pushes its own commits*, below).

The shared clone's `credential.helper` is `!gh auth git-credential`, so `jj git push`
authenticates as your role's App from the same file your `gh` reads. Never handle a token.

Then open the pull request with `gh pr create`. The PR body **must** contain the
line `Dispatch: <KEY>` — the daemon's fallback link from a PR to its Dispatch issue when the
branch name alone is ambiguous. Your pane's `gh` and `git` provide the GitHub
identity; never export, fetch, or replace a token. Other phases advance the existing branch
rather than creating a replacement bookmark or PR.

## PR body, review, and the merge gate

The implementer writes the pull request body from the template when it opens the pull request,
and every later phase edits its own lines of the live body rather than replacing it. Each proof
(the implementer's `E2E (implementer)` line and `proof` array, the tester's `E2E (tester)` line
and `proof` array) is the changed behaviour exercised on a production-like surface, recorded as
its command or run id, what was observed, the head SHA, and one negative control; a unit test is
never one. **Before you write or edit any line of the PR body or any `proof` array, read
`skill://legion-worker/references/pr-body.md`**: the template (the sole definition of the CI
line), the full definition of a proof, what the tester verifies, and the simplify pass.

- **Review threads** are answered and resolved one by one, never in bulk, and never one you have
  not read: one disposition reply per thread, then that thread's own resolution by the
  implementer, the pull request's author — its `gh api graphql` mutation after every push that
  answers a review, before its completion, for the threads it answered and for the bot threads the
  reviewer names to it as accepted (the review App cannot resolve one):
  `skill://legion-worker/references/review-threads.md`.
- **No deferrals.** A finding that changes
  behaviour, hides an error, or breaks a gate is fixed in this pull request; naming, duplication,
  or wording cleanup is batched into the one `Fast-follow:` line instead of iterating per push.
- **A red CI job** that failed on its own is re-run with
  `gh run rerun <run-id> --failed`, never by pushing a new commit or bringing in the base.
- **A conflict or a retarget** is the only reason to bring the base into the branch, always as a
  forward merge and never `jj rebase`; the unchanged-diff fingerprint each role compares
  afterwards is in the same reference: `skill://legion-worker/references/conflicts-and-rewrites.md`.
- **The merge gate**, in order: the tester's evidence green → the reviewer's approval of the head
  → retro → the merger's READY → the human merge → the implementer's production check. A person
  merges after READY under the repository's branch-protection and code-owner rules. The reviewer's
  submissions, retro's commit, the merger's READY, and the production check follow
  `skill://legion-worker/references/merge-gate.md`.
- **No surface reaches the changed path** is a report to the architect, never a reason to
  complete the phase: `skill://legion-worker/references/pr-body.md`.

## Completion gate: handoff write, verification, and persistence

The merger writes no handoff and pushes nothing, so this gate does not apply to it
(`packages/daemon/internal/prompts/roles/merger.md`).

A planner picking up after the issue moves back from implementing starts fresh on the remote
tip — `jj -R "$LEGION_WORKSPACE" new legion/<KEY>@origin` — since the implementer's unpushed
commits are no longer in the chain (*Every role pushes its own commits*, below).

Write the handoff, `$LEGION_WORKSPACE/.legion/<issue>/<phase>.json` (`plan`, `implement`, `test`
or `review`), with the ordinary `write` tool: a JSON object with `schemaVersion: 1`,
`phase: "<phase>"`, `issue: "<KEY>"`, `completed: "<RFC 3339 UTC time of writing>"`, and the
phase's own fields exactly as your role prompt spells them — the planner's `requiredSkills`,
`gapAnalysis`, `planReview` and `specDepartures` records; the implementer's `proof` array; the
tester's `implementerProof`, `failures` and, whenever it reports no failure, its own `proof`; the
reviewer's `verdict`, counts and `keyFindings`. Nothing stamps the first four for you. Each `proof`
entry, in either phase, is an object of six non-empty strings: `criterion` (the acceptance line it
proves), `surface`, `command`, `observed`, `headSha` (the commit it ran at) and `negativeControl`.
A handoff built from the one already on disk (a test handoff that accumulates review rounds) is
read with `read`, edited, and written whole with `write`, its `completed` set to now.

Then commit that exact handoff file onto the issue branch:

```bash
cd -- "$LEGION_WORKSPACE" && \
  jj -R "$LEGION_WORKSPACE" split -m "<phase>: record handoff" .legion/<issue>/<phase>.json
```

Then push (below) and report completion: call the `legion` tool with `op: "handoff_complete"` and
`summary` (two sentences for the architect; the tester adds `verdict: "pass"` or `"fail"`, the
merger `ready: true`). **Push before you complete is a hard rule.** `handoff_complete` finds, with
your pane's jj, the newest commit on the issue branch that carries `.legion/<KEY>/<phase>.json`
and reports it to the daemon, and refuses — posting nothing — while:

- the file has a change still in the working copy: commit it, then complete again;
- the file is missing from the workspace: write it, then commit and push it;
- the file is not committed on this branch (only the base carries it): write and commit it;
- the carrying commit was authored by another App (every role of an issue shares the workspace):
  run `jj -R "$LEGION_WORKSPACE" new`, then write and commit this phase's handoff again;
- the carrying commit is not yet on `legion/<KEY>@origin`: push the issue branch, then complete
  again.

Nothing checks the file's shape: the fields your role prompt spells are what the next role reads.
The daemon reads no handoff file and no branch head; it refuses, before recording anything:

- `HANDOFF_NOT_NEW` — the commit is the one you reported for your previous phase: write and
  commit this phase's handoff before completing.
- `READY_HEAD_CARRIES_HANDOFFS`, `READY_HEAD_CONFLICTS` and `READY_CHECKS_NOT_GREEN` — the
  merger's READY, in `skill://legion-worker/references/merge-gate.md`.
- `GITHUB_READ_FAILED` — GitHub's failure, not the head's, reading READY's checks: complete again.

Such a refusal records nothing, so the corrected call at the same commit is applied. The answer
may carry a `note` (READY published on an already-merged pull request, or a base requiring no
check).

Retro's last commit removes `.legion/<issue>/` from the head a human merges (`skill://legion-retro`);
a round after it, such as one a withdrawn READY sends back, writes and commits its own handoff
again, since `handoff_complete` reports the commit carrying the file. Retro, the merger and the
post-merge production check write no `.legion/<issue>/<phase>.json`, commit no handoff, and report
with `handoff_complete` alone, which reports the commit the workspace stands on.

**Every role pushes its own commits.** After the handoff commit — and, for the tester, the red
tests it wrote — push the issue branch with the plain `jj` of your pane, from bash in your
workspace; `<KEY>` is your `LEGION_ISSUE`:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> -r @- && jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<KEY>
```

A push whose commits touch only `.legion/` (a handoff-only push) ends its head's commit message
with `skip-checks: true` as the last line, so GitHub starts no workflow run for it and the daemon
carries the code head's verdict to it. Write that line with trailers off for the one describe,
since the pane's jj overlay would otherwise append `Omp-Session:` after it and GitHub honours the
trailer only as the last line:

```bash
jj -R "$LEGION_WORKSPACE" describe -r @- --config 'templates.commit_trailers=""' -m "$(jj -R "$LEGION_WORKSPACE" log -r @- --no-graph -T description)"$'\n\nskip-checks: true'
```

Every other push — code, red tests, `docs/solutions/`, retro's removal of `.legion/<KEY>/`, a
forward merge, a rewrite — carries no such line and none of GitHub's bracket keywords
(`[skip ci]`, `[ci skip]`, `[no ci]`, `[skip actions]`, `[actions skip]`), which start no workflow
wherever they stand. The daemon classifies a push by the paths its commits touch, never by the
trailer: a wrong trailer stalls the issue (a trailered code push gets no verdict carried; a head
with no CI is refused at READY) and never passes it. While the head commit carries
`skip-checks: true`, a pull-request body edit alone starts no GitHub workflow run, so a check that
re-judges the body re-runs on that head only after a later push; on a code head the same edit
re-runs it at once. While the pull request conflicts with its base (GitHub shows it
`CONFLICTING`), no workflow triggered `on: pull_request` runs for any of its activity types, so
a body edit re-judges nothing there either; a push cures both, but only once the pull request is
mergeable, so the implementer forward-merges a conflicting one first (*Reintegrating the base* in
`skill://legion-worker/references/conflicts-and-rewrites.md`).

Before the push, inspect `jj -R "$LEGION_WORKSPACE" log -r 'ancestors(@, 5)'` and run the
identity check above: the chain carries every earlier phase's pushed commits, and pushing them
with yours is expected. `jj git push` refuses a remote branch that moved because another role
pushed: report the refusal to the architect with its output, never force. A handoff commit never
sits on an implementer's unpushed chain: when the issue moves back to planning, the implementer's
unpushed commits stay off the bookmark until the implementer returns, and the planner writes its
handoff on the remote tip. The one sanctioned non-fast-forward move is the rewrite of a commit you
already pushed — a `jj squash --into` one, or any other rewrite — and before it, read *Rewriting
pushed commits* in `skill://legion-worker/references/conflicts-and-rewrites.md`: its guard checks
that no other tree's work is built on that commit, and only after it clears is the bookmark moved
with `--allow-backwards`. The merger makes no commit and pushes nothing.

## Completion: report to the architect, then stay

Report completion to the architect: call the `legion` tool with `op: "handoff_complete"` and
`summary`: two sentences for the architect. The tool is your only way to the daemon
(`handoff_complete`; `read_record`, your issue's record, which is what "re-read your issue
record" means; and `request_backward_move`), and nothing of yours runs `legion` from bash. The
tool call is what the extension's phase stall records, and a turn that ends with the phase still
open gets one reminder; do not complete again on that reminder when your first completion was
accepted — it stands, and a second is refused because the issue has already left your phase.

This publishes your phase's completion to the architect's role and clears the daemon's
record of this issue's active phase. Do not add pipeline labels, run a controller loop, or
invent a different completion protocol — this is the whole contract.

A reviewer's phase ends with its completion, not with its review; the order of a review round
(the handoff push; for an approval, CI settled green at that head; the review of that head; then
the completion) is in `skill://legion-worker/references/merge-gate.md`.

**A refused completion is information, not a retry loop.** The daemon attributes your report to
the run whose task you took, and answers with what it found. What each answer carries, and what to
do:

- `HANDOFF_STALE_GENERATION` — names the issue, the run it is on, and the run your completion
  reported. The issue has moved to a newer run since your task was given, so the work you just
  reported belongs to a run that is over. Nothing you can repeat changes that: stop, push nothing
  further, and tell the architect what you completed and that its run has been superseded. A task
  for the current run arrives in this same session if the phase still needs you. The same answer
  comes when your turn started before the daemon recorded the current run's task as yours, so it
  still holds you to the earlier run: that task is sent again. When a task arrives, do what it
  asks; if the work it asks for is already committed, call `handoff_complete` again, and never redo
  the work or write a second handoff.
- `HANDOFF_NOT_CURRENT_PHASE` — names your role, the issue, and the phase it is in now. The issue
  has left your phase; report to the architect rather than completing again.
- `HANDOFF_NO_RUN` — names neither: it says this claim has taken no task, so the daemon cannot
  tell which run you are reporting. Your pane is completing outside any assignment. Say so to the
  architect; do not re-run the phase. The same answer comes when your turn started before your task
  reached you: a notice or a message started it, and the task, refused while that turn ran, is sent
  when the turn ends. When a task arrives, do what it asks; if the work it asks for is already
  committed, call `handoff_complete` again, and never redo the work or write a second handoff.
- `HANDOFF_ALREADY_RECORDED` — names your role, the phase, the review round and the commit. This
  exact call was received before, and its first answer stands: accepted, or a refusal the daemon
  records with the call — `HANDOFF_STALE_GENERATION`, `HANDOFF_NOT_CURRENT_PHASE`,
  `READY_REQUIRED` or `HANDOFF_NOT_NEW`. `HANDOFF_NO_RUN` is never that first answer, since it is
  given before anything is recorded. Sending it again changes nothing; if you did not see that
  first answer, tell the architect so and quote this one.

Quote the answer verbatim in what you tell the architect: with the run and phase it names, the
difference between "my work is lost" and "my work belongs to the previous run" is visible.

**Stay in this session afterward.** Your process does not exit when your phase completes; it
stays live until your issue leaves the workflow. No move between phases stops it, and only an
explicit stop does: the issue closing; a move to `backlog`, `icebox` or `triage` (a child by a
person or its architect's `park_child`, or the root, which stops the whole tree); a child set back
to `todo` (which stops the interrupted phase's worker, not earlier roles); or an operator stop.
A stop keeps your session, and a crash relaunches it. Your next assignment arrives in this same
session. Other roles on
this issue may reach you through Envoy with
questions about the work you did — answer them, reading `$LEGION_WORKSPACE` and your own
committed handoff as needed, without mutating anything (see Workspace and handoff
precedence above). You will also be the one resumed, with a new prompt in this same
session, if this phase's work needs to run again.

When blocked on a product, scope, design, lifecycle, or cross-phase decision, `envoy_publish` the
owning architect a concise message: issue, phase, verified observation, what you tried, and the
decision required.

Never yield while blocked on a decision someone else owns. Before you stop, make the block visible
where its owner will see it: a product, scope, design, lifecycle, or cross-phase decision goes to
the owning architect as above, and a standalone human to-do goes in `dispatch ask`. Otherwise
proceed: proceeding is the default, and a phase that stops silently holds its issue until someone
notices.
