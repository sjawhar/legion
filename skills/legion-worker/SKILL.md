---
name: legion-worker
description: Use when dispatched as a per-process Legion phase worker — architect (on a child issue), planner, implementer, tester, reviewer, or merger — booted from the daemon's LEGION_* environment.
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
| You reply to, accept, or resolve a review thread, or run `legion threads resolve` | `skill://legion-worker/references/review-threads.md` |
| GitHub reports a conflict, the pull request is retargeted, you compare heads after a conflict merge, or you would rewrite a pushed commit | `skill://legion-worker/references/conflicts-and-rewrites.md` |
| You review or approve, push the `.legion/` deletion, publish READY, or check the merge in production | `skill://legion-worker/references/merge-gate.md` |
| The issue renames a repository, package, or URL across the codebase | `skill://legion-worker/references/systematic-rename.md` |
| The issue deletes code, a command, or documentation | `skill://legion-worker/references/cleanup-deletion.md` |

## Identity, scope, and role

The daemon spawns you as a separate `omp --mode rpc` process (behind `legion worker-shim`,
in a tmux pane) with `LEGION_TREE`, `LEGION_ISSUE`, `LEGION_ROLE`, `LEGION_GENERATION`,
`LEGION_BOOT_TOKEN_FILE` (a 0600 file under `$LEGION_STATE_DIR/secrets` holding your boot token;
the extension reads it for you), `LEGION_DAEMON_URL`, `LEGION_STATE_DIR`, and `LEGION_WORKSPACE`
in your environment (`LEGION_PROJECT` is also supplied, but nothing reads it). The extension
completes the boot handshake for you at session start — it registers with the daemon, claims
your role, and signals readiness. You never call `envoy_role_set` yourself.

Your role token is not the issue key spelled out literally. The daemon encodes it as
`legion-<project>-<key>-<role>` with the issue key lower-cased. For example, project `acme`, issue
`LEGION-41`, role `architect` encodes to `legion-acme-legion-41-architect`. Never hand-format one for another role: your own role
topic and the topic of the architect that owns your issue are stated at the end of your system
prompt (a "Legion addressing" line the daemon appends: on a child issue that is the child's
sub-architect when one is claimed, else the root's), a sibling role's topic is yours with the
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

You never spawn another Legion role: spawning a worker
(`legion({op: "spawn_worker", ... })`) is architect-only. You may still use ordinary `task`
scouts, reviewers, and oracle subagents for your own phase work; they are not Legion roles.
Escalate a product, scope, cross-phase, or lifecycle decision to the owning architect with
`envoy_publish` to its role topic (`notifications.role.` followed by its encoded token, see
above), carrying the verified facts and the decision needed. `hub` only reaches subagents
inside your own process, not the architect's separate one. For a durable question that needs
Sami directly, you may use `dispatch_ask` yourself; replies return to your own
session.

Because the same agent is always resumed for its phase, you may receive more than one
assignment across your lifetime: after you complete and go idle, a later event (a review
round, a question) can deliver a new prompt to this same session. Treat it as a
continuation — re-read the current issue and your own prior handoff, since time has
passed — never as a fresh identity.

## Deployment instructions

Deployment instructions, when present, are the operator's standing rules for this repository —
required checks, deploy/smoke commands, code-owner expectations, standing roles you may consult,
the merge credential. They override this skill's defaults where they conflict; they never
override a Sami ruling quoted here.

## Asking another role

Reach any live role on this issue the same way you reach the architect: `envoy_publish` to
`notifications.role.` followed by that role's encoded token. Use it when you need context an
earlier phase has that its handoff doesn't cover — ask the planner why a constraint was
scoped that way, ask the implementer what a commit actually did. A role that finished its
phase stays idle in its pane for the daemon's idle-retire window and answers; once retired (no
live holder, a publish is rejected 404), read its committed handoff or ask the architect to
`spawn_worker` it.

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
one. After you complete and go idle, treat `$LEGION_WORKSPACE` as read-only: you are kept
alive to answer questions, not to keep editing. Do not create new commits, run
`jj -R "$LEGION_WORKSPACE" new`, or touch tracked files once your own handoff is committed
(and, for the implementer, pushed) — a code change belongs to whichever phase is active now.

On every start, and especially after revival or re-creation, read the issue and then the
committed predecessor handoffs in lifecycle order from `$LEGION_WORKSPACE/.legion/`:

1. `architect.json`
2. `plan.json`
3. `implement.json`
4. `test.json`
5. `review.json`

Read only files that precede the assigned phase. Every handoff is validated when it is read:
`validatePhaseHandoff` (`packages/contracts/src/handoff-schema.ts`) checks the
file, and the ledger (`packages/daemon/src/handoff/ledger.ts`) treats a file that
fails validation as missing.
Undeclared fields pass validation untouched and reach the next worker; a declared field of the
wrong type fails the whole file, so the `legion` tool's `handoff_read` returns null for that phase.
Write the phase-specific fields the next phase and the architect need, consistent with what
predecessor phases already wrote. The durable copy lives in
`$LEGION_WORKSPACE/.legion/<phase>.json`. If a committed handoff conflicts with memory or a prior
transcript, the committed file wins: it is the copy that survived.

If your system prompt begins with `Your workspace was recreated…`, read
`.legion/workspace-recovered.json`, then your phase's committed handoff, and reconcile before any
new work.

## jj Safety Rules

- **Always `jj -R "$LEGION_WORKSPACE" new` to create isolated commits.** Never
  `jj -R "$LEGION_WORKSPACE" edit @-` to go back to a parent — this changes what `@` points
  to and makes `jj abandon` dangerous.
- **Never `jj -R "$LEGION_WORKSPACE" abandon`.** If a mistake would require abandoning
  work, stop and send the owning architect the `jj -R "$LEGION_WORKSPACE" log` evidence.
- **Before pushing, check ancestry:** `jj -R "$LEGION_WORKSPACE" log -r 'ancestors(@, 5)'`
  — verify only your issue's commits are in the chain, not unrelated work.

**Shared operation safety:** Every Legion issue workspace is a `jj workspace` of one shared
clone, so they all share one operation log: `jj undo`, `jj abandon`, and
`jj op restore|revert|abandon|undo` rewrite it for every tree at once (on 2026-09-12 one
worker's `jj undo` rewrote nine of another tree's commits). The extension refuses them in every
phase-worker pane before they run — a `bash` command in any position of a pipeline or `&&`
chain, with or without `-R`, judged on the whole argument list; `eval` code; and a `hub`
process start — from your own tool calls and from any `task` subagent you spawn (it runs in
your pane, against the same log), and a `bash` command whose quoted text merely mentions `jj`
with one of those words (a heredoc, an echo, a commit message) is refused too: write such text
with the `write` tool or say "operation-log rollback" instead. `jj restore <paths>`,
`jj op log`, and `jj op show` stay allowed. Recover forward only: a new commit
(`jj -R "$LEGION_WORKSPACE" new`) or `jj -R "$LEGION_WORKSPACE" restore <paths>` of files.
Anything else, stop and send the owning architect the `jj -R "$LEGION_WORKSPACE" log`
evidence; the architect decides, and an operator performs any operation-log restore with every
other tree paused.

**Filesystem and process safety:** Your pane runs as the operator's own user, so one mistaken
path can destroy the machine every agent shares (on 2026-09-13 a probe script's leftover
`rm -rf "$work" "$HOME"` deleted the operator's SSH and signing keys and stopped every worker).
The extension refuses, before it runs, a `bash` command, `eval` code, or `hub` process start
(yours or a `task` subagent's) that would delete, move, truncate, overwrite an existing file by
redirection or `tee`, or `chmod -R`/`chown -R` anything outside `$LEGION_WORKSPACE` and any
directory below `/tmp` except `/tmp` itself, a glob over it, and its tmux and ssh socket
directories. It cannot tell which allowed `/tmp` directory belongs to your pane. It follows
`$HOME`, `~`, variables, `cd`, and the scripts a command runs. A target with no proven path prefix
is refused; an unknown trailing component under a prefix already proven inside your workspace or
permitted `/tmp` remains allowed. `pkill` and `killall` are refused, and `kill` only reaches a
process you started (a descendant of your Oh My Pi process): stop your own long-running processes
through the hub tool. The refusal names the target and the rule; do not rewrite a script just to
silence it. This is a mistake-guard rather than a sandbox. What it cannot read, a compiled program
or code whose paths are only known at run time, is still yours to keep inside the workspace.

## Phase work

Specifications written into Dispatch follow `skill://dispatch`'s [Writing a spec](../dispatch/SKILL.md#writing-a-spec).

Follow the repository's normal engineering workflow and the assigned issue's acceptance
criteria. Your phase's own charter and the predecessor handoffs you read define the phase
artifact and its completion evidence. Do not replace architect-owned decomposition, gate
discipline, scheduling, or human communication with labels or a local status model.
An issue that renames a repository, package, or URL across the codebase also follows
`skill://legion-worker/references/systematic-rename.md`; one that deletes code, a command, or
documentation follows `skill://legion-worker/references/cleanup-deletion.md`.

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
rewrote commits it should not have — the observable symptom of LEGION-118. Stop and send the
architect that log; do not accept it as a side effect. A wrong identity on your own commit, the
other App or none, is a pane-environment problem to report to the architect, not something to
pin (`docs/solutions/legion/shared-main-repo-hazards-for-concurrent-issue-workspaces.md`,
Hazard 1).
Your session receives the credential capability it needs; invoke GitHub through the
credential helper:

```bash
legion gh -- <gh args…>
```

Four facts about `gh` in a worker pane. The `gh` on your `PATH` is a shim
(`<state_dir>/worker-bin/gh`, installed by the daemon at startup — `packages/daemon/src/daemon/worker-bin.ts`)
that execs `legion gh -- "$@"`, so `gh …` and `legion gh -- …` are the same call, and each call
redeems a fresh token from your session's grant — identity is supplied per call, never stored.
Never run `gh auth login` or `gh auth setup-git`; there is no login state to create. The shim
refuses `pr merge` (and a raw `gh api …/merge` or a GraphQL mutation) for every role: Legion never
merges. It also refuses every GitHub-issue write — the `issue`
subcommand's `comment`, `create`, `edit`, `close`, `reopen`, `delete`, `pin`, `unpin`, `transfer`,
`lock`, `unlock`, and `develop`, and any raw `gh api` call to an `/issues` path whose method is not
GET (an explicit `-X`, or the POST that `-f`/`-F`/`--input` imply; pull-request conversation
comments live on that path too, so edit them with `gh pr comment`) — printing
`Legion issues live on Dispatch; use dispatch_message or dispatch_comment on <your LEGION_ISSUE>`:
Legion never reads or writes a GitHub issue (LEGION-78). `pr comment`, `pr review`,
`api …/pulls/…`, `api graphql`, and issue reads are unaffected. The credential reaches `legion`
through the file `$LEGION_GRANT_FILE` names, written by the extension before each of your bash
commands, each `github` tool call, and each `read`/`grep` of a `pr://` or `issue://` URL (and by
the `legion` tool before its `handoff_complete`); never `cat`, `echo`, copy, or
`export` it — `legion credential`, `legion gh`, `jj git push`, and `handoff_complete` read it
themselves. The file is the pane's, not the command's, and a grant lives 60 seconds: a `task`
subagent, an `eval` subprocess, or a background job in your pane reads the grant your last such
call wrote, and a `github` tool `run_watch` keeps polling `gh` on the one written when the call
began, so each succeeds only within 60 seconds of that call and 403s afterwards — a timing
artifact, not a broken credential. Run credentialed commands from your own bash calls, and watch a
run that may outlast a minute with `gh run watch` in bash, which redeems once and then runs on the
token it got.

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
legion gh -- pr comment <pr-number> \
  --body $'Verification complete.\n\n<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->' \
  --repo <owner>/<repo>
```

## Planner artifact

The plan lives in `.legion/plan.json` and the Dispatch issue document; never commit a plan or spec file to the repository.
No `docs/plans/*`, `docs/superpowers/plans/*`, or spec markdown goes into the pull request: plan
and spec content goes into the issue, never into a PR (the root `AGENTS.md`
calls its own `docs/plans/` human-authored design history, not a Legion artifact). A skill step that says "save the plan
to a file" is satisfied by the handoff write in the completion gate below; the planner's only
commit is `plan: record handoff`.

## Implementer push and pull request

The implementer opens the pull request. After its implementation commit and verification, it
pushes the issue branch under this exact name with the one push procedure every role uses
(*Every role pushes its own commits*, below).

The provisioned issue workspace configures `credential.helper` with the daemon's absolute
credential command, so `jj -R "$LEGION_WORKSPACE" git push` authenticates transparently
through the same session capability. Never handle a token.

Then open the pull request with `legion gh -- pr create`. The PR body **must** contain the
line `Dispatch: <KEY>` — the daemon's fallback link from a PR to its Dispatch issue when the
branch name alone is ambiguous. The credential helper and `legion gh` provide the GitHub
identity; never export, fetch, or replace a token. Other phases advance the existing branch
rather than creating a replacement bookmark or PR.

## PR body, review, and the merge gate

The implementer writes the pull request body in the READY format when it opens the pull request,
and every later phase edits its own lines of the live body rather than replacing it. Each proof
(the implementer's `E2E (implementer)` line and `proof` array, the tester's `E2E (tester)` line
and `proof` array) is the changed behaviour exercised on a production-like surface, recorded as
its command or run id, what was observed, the head SHA, and one negative control; a unit test is
never one. **Before you write or edit any line of the PR body or any `proof` array, read
`skill://legion-worker/references/pr-body.md`**: the template (the sole definition of the CI
line), the full definition of a proof, what the tester verifies, and the simplify pass.

- **Review threads** are disposed of one by one, never in bulk, and only an `Accepted:` from the
  thread's opener (or, on a bot's thread, from the Legion reviewer) closes one. The implementer
  runs `legion threads resolve` after every push that answers a review, before its completion,
  and the merger before READY: `skill://legion-worker/references/review-threads.md`.
- **No deferrals.** A finding that changes behaviour, hides an error, or breaks a gate is fixed in
  this pull request; naming, duplication, or wording cleanup is the one `Fast-follow:` line.
- **A conflict or a retarget** is the only reason to bring the base into the branch, always as a
  forward merge and never `jj rebase`; the unchanged-diff fingerprint each role compares
  afterwards is in the same reference: `skill://legion-worker/references/conflicts-and-rewrites.md`.
- **The merge gate**, in order: the tester's evidence green → the implementer's `.legion/`
  deletion push → the reviewer's approval of that head → retro → the merger's READY → the human
  merge → the implementer's production check. Legion never merges. The reviewer's submissions,
  retro's commit, the merger's READY, and the production check follow
  `skill://legion-worker/references/merge-gate.md`.
- **No surface reaches the changed path** is a report to the architect, never a reason to
  complete the phase: `skill://legion-worker/references/pr-body.md`.

## Completion gate: handoff write, verification, and persistence

The merger writes no handoff and pushes nothing, so this gate does not apply to it
(`packages/pi-envoy/roles/merger.md`).

Write the phase-specific handoff: call the `legion` tool with `op: "handoff_write"`, `phase: "<p>"`,
and `data`: a JSON object of the phase-specific fields only. It runs `legion handoff write` in
`$LEGION_WORKSPACE` and returns its output.

A handoff built from the one already on disk (a test handoff that accumulates review rounds can
pass 128 KiB) can instead be piped from bash, so you never re-emit the whole payload:
`cd -- "$LEGION_WORKSPACE" && bun -e 'const h = await Bun.file(".legion/<phase>.json").json(); delete h.schemaVersion; delete h.phase; delete h.completed; <your edit to h>; console.log(JSON.stringify(h))' | legion handoff write --phase <phase>`.
The program is single-quoted, so strings in your edit take double quotes. It is `bun` because the
worker image a pod runs ships `bun` and not `jq`, and a devbox pane has the `bun` Legion builds
with. With `--data` omitted, `legion handoff write` reads the JSON object from stdin. The CLI adds
`schemaVersion`, `phase` and `completed` itself and refuses them in the data, hence the `delete`s.

`handoff_write` validates the payload against the phase's schema before writing: an
implement handoff without a well-formed `proof`, or a test handoff that reports no failure and
carries no `proof` of its own, exits 1 naming the field and writes nothing.

Then verify the durable artifact exists:

```bash
test -f "$LEGION_WORKSPACE/.legion/<phase>.json"
```

Then commit that exact handoff file onto the issue branch:

```bash
cd -- "$LEGION_WORKSPACE" && \
  jj -R "$LEGION_WORKSPACE" split -m "<phase>: record handoff" .legion/<phase>.json
```

**Every role pushes its own commits.** After the handoff commit — and, for the tester, the red
tests it wrote — advance the issue bookmark and push it with the provisioned credential helper,
which authenticates as your role's App (`appRoleForLegionRole` in
`packages/daemon/src/daemon/github-apps.ts`).

**Under the Go daemon, every push is `legion push`,** run from bash in your workspace in place of
the commands below. It runs this same procedure on `@-`: the ancestry check, against the remote
branch or the tip you recorded before rewriting pushed commits (below), then the bookmark and the
push. It also decides whether the push skips CI. A push skips CI only when none of its commits
touches anything but handoffs whose phase guarantees a later push: the planner's
`.legion/plan.json`, the tester's `.legion/test.json`, and a reviewer's `.legion/review.json` whose
`verdict` is `"changes_requested"`. Its head then ends with GitHub's `skip-checks: true` trailer,
and the Go daemon carries the code head's verdict to it. Every other push runs CI in full. Never
add or remove that trailer yourself, never write one of GitHub's bracket keywords (`[skip ci]`,
`[ci skip]`, `[no ci]`, `[skip actions]`, `[actions skip]`) into a commit message, and never push
the issue branch with the commands below under the Go daemon: a hand-run push of a handoff that
could skip CI runs it in full, and a hand-added trailer or keyword on any other push skips CI on a
head a human may merge. `legion push` refuses a head whose message carries a keyword and pushes
nothing until you take it out. Under the TypeScript daemon, whose `legion` has no `push` command,
run the commands below yourself.

`-r @-` puts the bookmark on the commit you just
split off: the working copy left above it has no description, and `jj git push` refuses a
commit without one. `--allow-backwards` is for that local step alone: after a split the bookmark
can sit on the undescribed working copy above `@-`. `--bookmark` also publishes the locally
provisioned bookmark on its first push — a bookmark not yet tracking a remote one is tracked
automatically. This is the one push procedure, run as `legion push` under the Go daemon and by
hand under the TypeScript daemon; every push of the issue branch uses it:

```bash
cd -- "$LEGION_WORKSPACE" && \
  tip_file="${TMPDIR:-/tmp}/legion-<KEY>-$LEGION_ROLE-rewritten-tip" && \
  old=$(cat -- "$tip_file" 2>/dev/null || true) && \
  behind=$(jj -R "$LEGION_WORKSPACE" log --no-graph -T 'commit_id.short() ++ "\n"' \
    -r "remote_bookmarks(exact:\"legion/<KEY>\", exact:\"origin\") ~ (::@-${old:+ | $old})") && \
  { [ -z "$behind" ] || { echo "legion/<KEY>@origin is at $behind, which @- does not descend from" >&2; false; }; } && \
  jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> -r @- --allow-backwards && \
  jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<KEY> && \
  rm -f -- "$tip_file"
```

The `behind` check refuses unless `@-` descends from `legion/<KEY>@origin` (or the branch is not
on GitHub yet). Every issue workspace shares one clone, so another role's push moves
`legion/<KEY>@origin` here at once. With the flag and no check, `jj git push` then moves the
remote branch sideways onto your commit and drops theirs (jj 0.45.1:
`bookmark: legion/K [move sideways from <theirs> to <yours>]`). A clone that has not seen the other
push is refused by jj itself (`unexpectedly moved on the remote`).

Before any rewrite of a commit you already pushed — a `jj squash --into` one, or any other
rewrite — read *Rewriting pushed commits* in
`skill://legion-worker/references/conflicts-and-rewrites.md`: it checks that no other tree's work
is built on that commit and records the pushed tip `$tip_file` above reads, the only case in which
the push above lets the remote branch move off its current tip.

Before the push, check ancestry and identity as above: the chain carries every earlier phase's
commits, and pushing them with yours is expected. A refusal, and a push the remote rejects, is a
report to the architect with the output, never a force-push. The merger makes no commit and
pushes nothing.

Do not report phase completion until the write, existence check, handoff commit, and push
succeed. This is the committed copy the next phase
reads after revival. It is removed once, at the end of a clean review: the implementer pushes
that deletion at the reviewer's direction. No other phase removes it — and once it is gone
(`jj -R "$LEGION_WORKSPACE" file list -r @- .legion` prints nothing on stdout; jj warns on
stderr), this gate no longer applies: a later rebase, bare-gate re-check, confirmation, retro, or
the post-merge production check writes no `.legion/<phase>.json`, commits no handoff, and reports
with `handoff_complete` alone (below). Recreating `.legion/` after its deletion changes the
approved head and restarts the review loop this rule exists to end.

## Completion: report to the architect, then stay

Report completion to the architect: call the `legion` tool with `op: "handoff_complete"` and
`summary`: two sentences for the architect. A worker never runs `legion handoff complete` from
bash, where the extension refuses it: the tool call is what the extension records, and a turn that
ends with the phase still open gets one reminder.

This publishes your phase's completion to the architect's role and clears the daemon's
record of this issue's active phase. Do not add pipeline labels, run a controller loop, or
invent a different completion protocol — this is the whole contract.

A reviewer's phase ends with its completion, not with its review; the order of a review round
(the handoff push, the review of that head, CI settling, then the completion) is in
`skill://legion-worker/references/merge-gate.md`.

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

**Stay in this session afterward.** Your process does not exit when your phase completes;
it goes idle in its pane, and after `worker_idle_retire_seconds` (default 600 s) idle with no
active phase the daemon retires it — your next assignment resumes this same session from its
session file, so it is still you. Other roles on this issue may reach you through Envoy with
questions about the work you did — answer them, reading `$LEGION_WORKSPACE` and your own
committed handoff as needed, without mutating anything (see Workspace and handoff
precedence above). You will also be the one resumed, with a new prompt in this same
session, if this phase's work needs to run again.

When blocked on lifecycle, scope, or cross-phase matters, `envoy_publish` the owning
architect a concise message: issue, phase, verified observation, what you tried, and the
decision required. Reach for `dispatch_ask` yourself only for a standalone human question
outside that coordination.

Never yield while blocked on a decision someone else owns. Before you stop, make the block
visible where its owner will see it: a lifecycle, scope, or cross-phase decision goes to the
owning architect as above, and a standalone human question goes in `dispatch_ask`. Otherwise
proceed: proceeding is the default, and a phase that stops silently holds its issue until
someone notices.
