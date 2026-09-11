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

This skill documents the target Dispatch-native contract. `LEGION_ISSUE` as a Dispatch key,
the `dispatch_ask` tool, and the `Dispatch: <KEY>` PR-body linkage land with PR B (#TBD);
until that PR merges, this skill's contract is not yet runnable on `main`.

## Identity, scope, and role

The daemon spawns you as a separate `omp --mode rpc` process (behind `legion worker-shim`,
in a tmux pane) with `LEGION_TREE`, `LEGION_ISSUE`, `LEGION_ROLE`, `LEGION_GENERATION`,
`LEGION_BOOT_TOKEN`, `LEGION_DAEMON_URL`, `LEGION_STATE_DIR`, and `LEGION_WORKSPACE` in your
environment (`LEGION_PROJECT` is also supplied, but nothing reads it). The extension
completes the boot handshake for you at session start — it registers with the daemon, claims
your role, and signals readiness. You never call `envoy_role_set` yourself.

Your role token is not the issue key spelled out literally. The daemon encodes it as
`legion-<project>-<KEY>-<role>`. For example, project `acme`, issue `LEGION-41`, role
`architect` encodes to `legion-acme-LEGION-41-architect`. Never hand-format one for another
role: your own role topic and your tree's architect's topic are stated at the end of your
system prompt (a "Legion addressing" line the daemon appends), a sibling role's topic is
yours with the trailing `-<role>` replaced, and the `roleToken` helper in `@legion/contracts`
computes any other one exactly the way the daemon does — prefer a topic you've already
been given before recomputing one.

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

## Asking another role

Reach any live role on this issue the same way you reach the architect: `envoy_publish` to
`notifications.role.` followed by that role's encoded token. Use it when you need context an
earlier phase has that its handoff doesn't cover — ask the planner why a constraint was
scoped that way, ask the implementer what a commit actually did. A role that finished its
phase is still alive and idle in its pane; it answers.

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
and pushed — a code change belongs to whichever phase is active now.

On every start, and especially after revival or re-creation, read the issue and then the
committed predecessor handoffs in lifecycle order from `$LEGION_WORKSPACE/.legion/`:

1. `architect.json`
2. `plan.json`
3. `implement.json`
4. `test.json`
5. `review.json`

Read only files that precede the assigned phase. There is no handoff schema (rejected
design — no schema validation runs anywhere in this pipeline): write the phase-specific
fields the next phase and the architect need, consistent with what predecessor phases
already wrote. The durable copy lives in `$LEGION_WORKSPACE/.legion/<phase>.json`. If a
committed handoff conflicts with memory or a prior transcript, the committed file wins: it
is the copy that survived.

## jj Safety Rules

- **Always `jj -R "$LEGION_WORKSPACE" new` to create isolated commits.** Never
  `jj -R "$LEGION_WORKSPACE" edit @-` to go back to a parent — this changes what `@` points
  to and makes `jj abandon` dangerous.
- **Never `jj -R "$LEGION_WORKSPACE" abandon`.** If a mistake would require abandoning
  work, stop and send the owning architect the `jj -R "$LEGION_WORKSPACE" log` evidence.
- **Before pushing, check ancestry:** `jj -R "$LEGION_WORKSPACE" log -r 'ancestors(@, 5)'`
  — verify only your issue's commits are in the chain, not unrelated work.

**Shared operation safety:** Never run `jj op restore` in a Legion workspace. It rewrites
the shared operation log. If a mistake reaches that point, stop and send the owning
architect the `jj -R "$LEGION_WORKSPACE" log` evidence; recover only through the approved,
path-scoped workflow.

## Phase work

Specifications written into Dispatch follow [`skills/dispatch`'s Writing a spec](../dispatch/SKILL.md#writing-a-spec).

Follow the repository's normal engineering workflow and the assigned issue's acceptance
criteria. Your phase's own charter and the predecessor handoffs you read define the phase
artifact and its completion evidence. Do not replace architect-owned decomposition, gate
discipline, scheduling, or human communication with labels or a local status model.

Commit attribution is automatic: the extension exports a `JJ_CONFIG` overlay when your
session starts, so every jj commit you make carries an `Omp-Session: <this-session-id>`
trailer with no action from you. Do not add attribution trailers by hand.

The jj configuration already supplies your phase's plus-addressed author and committer
identity. Do not override Git identity configuration. Your session receives the credential
capability it needs; invoke GitHub through the credential helper:

```bash
legion gh -- <gh args…>
```

## GitHub PR comment attribution

Append this exact structured footer to **every** pull-request comment and review that this
phase posts on GitHub. It preserves session provenance on the artifact itself so work stays
attributable to the session that produced it. Dispatch comments carry session provenance
natively through their own `actor`/`origin` fields; this footer is only for GitHub PR
artifacts:

```html
<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->
```

For example:

```bash
legion gh -- pr comment <pr-number> \
  --body $'Verification complete.\n\n<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->' \
  --repo <owner>/<repo>
```

## Implementer push and pull request

Only the implementer creates the issue bookmark, pushes it, and opens the pull request.
After its implementation commit and verification, it uses this exact branch name and push
procedure:

```bash
cd -- "$LEGION_WORKSPACE" && \
  jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> && \
  jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<KEY>
```

The provisioned issue workspace configures `credential.helper` with the daemon's absolute
credential command, so `jj -R "$LEGION_WORKSPACE" git push` authenticates transparently
through the same session capability. Never handle a token.

Then open the pull request with `legion gh -- pr create`. The PR body **must** contain the
line `Dispatch: <KEY>` — the daemon's fallback link from a PR to its Dispatch issue when the
branch name alone is ambiguous. The credential helper and `legion gh` provide the GitHub
identity; never export, fetch, or replace a token. Other phases advance the existing branch
rather than creating a replacement bookmark or PR.

## PR body and merge-queue discipline

The implementer writes the PR body in the merge queue's READY format from the moment the
PR opens, and every later phase keeps it current rather than replacing it:

```
## Verification

**CI:** `pr-checks-result` run <run-id> — success at <head-sha>.

**Threads:** <n> resolved, 0 unresolved. Each disposed individually, never in bulk:
- Thread <id>: fixed in <commit-sha> — <one line>.
- Thread <id>: not a defect — <reason>.

**Thermo:** thermonuclear-deep-review + thermonuclear-code-quality run once at <head-sha>:
<verdict>. (omitted entirely on a docs-only PR — no thermo pass runs)

**E2E:** <surface> — ran `<command or run id>`, observed <result>, at head <sha>.
Negative control: <deliberately broken input> → <refusal or failure observed>.

**Fast-follow:** <one named cleanup item and where it will land>, or "none".

**Chain:** stacked on <base bookmark> frozen at <sha> / not stacked.
```

- **Threads are dispositioned individually, never resolved in bulk.** Every open review
  thread gets its own line naming the fixing commit or the reason it isn't a defect before
  it is marked resolved.
- **Correctness fixes land in this PR; cleanup is one named fast-follow.** A finding that
  changes behavior, hides an error, or breaks a gate is fixed here — never deferred.
  Findings about naming, duplication, or wording are batched into the single `Fast-follow`
  line instead of iterating per push.
- The tester fills in the `E2E` section: the real surface a user reaches the criterion
  through, the exact command or run id, what was observed, the head SHA, and one negative
  control — a deliberately broken input and the refusal or failure it produced. A unit or
  integration test is a regression lock, never proof of a criterion.
- The reviewer verifies the `CI`, `Threads`, and `E2E` facts against GitHub directly —
  never from a handoff — then runs `task(agent="thermonuclear-deep-review")` and
  `task(agent="thermonuclear-code-quality")` once at that head and records the verdict.
  Skip the `Thermo` line entirely on a docs-only PR. Post every correctness finding as a
  PR review comment and return the issue to the architect; when clean, have the architect send
  the implementer back to push the `.legion/` deletion (the review App cannot push), then
  review **that** head and approve it by name.
- Once a base is frozen for others to stack on, never rewrite it — fixes land as new
  commits on top, and the `Chain` line records what is frozen.
- The merger confirms the approved head still equals the current head, then publishes
  `READY #<n> at <sha>` plus the PR body's gate facts to the merge queue's role
  (`notifications.role.pr-queue`) with `envoy_publish`. The merger never merges; the queue
  merges under its own authority.

## Completion gate: handoff write, verification, and persistence

Write the phase-specific handoff:

```bash
cd -- "$LEGION_WORKSPACE" && \
  legion handoff write --phase <p> --data '<JSON object of phase-specific fields only>'
```

Then verify the durable artifact exists:

```bash
test -f "$LEGION_WORKSPACE/.legion/<phase>.json"
```

Then commit that exact handoff file onto the issue branch:

```bash
cd -- "$LEGION_WORKSPACE" && \
  jj -R "$LEGION_WORKSPACE" split -m "<phase>: record handoff" .legion/<phase>.json
```

If the issue bookmark exists locally, advance it and push it with the provisioned
credential helper. `--bookmark` also publishes the locally provisioned bookmark on its
first push — a bookmark not yet tracking a remote one is tracked automatically:

```bash
cd -- "$LEGION_WORKSPACE" && \
  jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> && \
  jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<KEY>
```

Do not report phase completion until the write, existence check, and handoff commit
succeed; when an issue branch exists, its push is also required. This is the committed
copy the next phase reads after revival. It is removed once, at the end of a clean review: the
implementer pushes that deletion at the reviewer's direction. No other phase removes it.

## Completion: report to the architect, then stay

Report completion to the architect with:

```bash
cd -- "$LEGION_WORKSPACE" && \
  legion handoff complete --summary '<two sentences for the architect>'
```

This publishes your phase's completion to the architect's role and clears the daemon's
record of this issue's active phase. Do not add pipeline labels, run a controller loop, or
invent a different completion protocol — this is the whole contract.

**Stay in this session afterward.** Your process does not exit when your phase completes;
it goes idle in its pane. Other roles on this issue may reach you through Envoy with
questions about the work you did — answer them, reading `$LEGION_WORKSPACE` and your own
committed handoff as needed, without mutating anything (see Workspace and handoff
precedence above). You will also be the one resumed, with a new prompt in this same
session, if this phase's work needs to run again.

When blocked on lifecycle, scope, or cross-phase matters, `envoy_publish` the owning
architect a concise message: issue, phase, verified observation, what you tried, and the
decision required. Reach for `dispatch_ask` yourself only for a standalone human question
outside that coordination.
