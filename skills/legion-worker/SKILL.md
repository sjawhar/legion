---
name: legion-worker
description: Use when dispatched as an architect, plan, implement, test, or review phase worker in a Legion issue workspace.
---

# Legion Phase Worker

You are one phase in a shared issue workspace, not a dispatcher or pipeline coordinator. The
architect owns the tree; phases use the same jj workspace sequentially. Complete the phase
assigned in the prompt, return its structured output to the architect, and leave the durable
copy that the next phase can trust.

## Identity, scope, and role

The dispatch supplies the issue, phase, daemon-minted role token, workspace, and task
`outputSchema`. At startup, claim that role with `envoy_role_set`; never construct a role
token from an issue name. A claim survives parking, and a revived or re-created worker claims
its own role again.

Read the current issue and its acceptance criteria before changing the workspace. Work only
on this phase's artifact. You may use ordinary scouts, reviewers, and oracle subagents for
phase work; never spawn legion-role workers. Escalate a product, scope, cross-phase, or
lifecycle decision to the owning architect through hub, with the verified facts and the
decision needed. For a durable question that needs Sami directly, you may use the raw
`dispatch` MCP tool yourself; replies return to your own session.

## Workspace and handoff precedence

The `workspace` attribute in your `<legion-spawn>` block is the authoritative issue
workspace. Before reading repository files or handoffs, you **MUST** bind to that exact
path with:

```bash
cd -- "<workspace>" && jj -R "<workspace>" status
```

Never rely on the inherited cwd. Every later repository shell command **MUST** begin
`cd -- "<workspace>" &&`; every jj command **MUST** use `-R "<workspace>"`; and native
filesystem tool paths **MUST** be absolute under that workspace. Do not create an isolated
worktree, change the workspace topology, or mix another issue's work into it. Concurrent
issues have disjoint workspaces; phases for this issue are sequential.

On every start, and especially after revival or re-creation, read the issue and then the
committed predecessor handoffs in lifecycle order from `<workspace>/.legion/`:

1. `architect.json`
2. `plan.json`
3. `implement.json`
4. `test.json`
5. `review.json`

Read only files that precede the assigned phase. The live path returns JSON matching the task
`outputSchema` directly to the architect. The durable path uses the **same schema** in
`<workspace>/.legion/<phase>.json`. If a committed handoff conflicts with memory or a prior
transcript, the committed file wins: it is the copy that survived.

## jj Safety Rules

- **Always `jj -R "<workspace>" new` to create isolated commits.** Never
  `jj -R "<workspace>" edit @-` to go back to a parent — this changes what `@` points to
  and makes `jj abandon` dangerous.
- **Never `jj -R "<workspace>" abandon`.** If a mistake would require abandoning work,
  stop and send the owning architect the `jj -R "<workspace>" log` evidence.
- **Before pushing, check ancestry:** `jj -R "<workspace>" log -r 'ancestors(@, 5)'` —
  verify only your issue's commits are in the chain, not unrelated work.

**Shared operation safety:** Never run `jj op restore` in a Legion workspace. It rewrites the
shared operation log. If a mistake reaches that point, stop and send the owning architect the
`jj -R "<workspace>" log` evidence; recover only through the approved, path-scoped workflow.

## Phase work

Follow the repository's normal engineering workflow and the assigned issue's acceptance criteria.
The dispatch output schema defines the phase artifact and completion evidence. Do not replace
architect-owned decomposition, gate discipline, scheduling, or human communication with labels
or a local status model.

Commit attribution is automatic: the Legion extension exports a `JJ_CONFIG` overlay at root
bootstrap, so every jj commit made in the session family carries an `Omp-Session: <root-session-id>`
trailer with no action from you. Do not add attribution trailers by hand.

The jj configuration already supplies the phase worker's plus-addressed author and committer
identity. Do not override Git identity configuration. The worker session receives the
credential capability it needs; invoke GitHub through the credential helper:

```bash
legion gh -- <gh args…>
```

## GitHub comment attribution

Append this exact structured footer to **every** GitHub issue comment, pull-request comment,
and review that this phase posts. It preserves session provenance on the artifact itself so
work stays attributable to the session that produced it:

```html
<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->
```

For example:

```bash
legion gh -- issue comment <issue-number> \
  --body $'Verification complete.\n\n<!-- legion: {"session":"<session-id>","phase":"<phase>"} -->' \
  --repo <owner>/<repo>
```

## Implementer push and pull request

Only the implementer creates the issue bookmark, pushes it, and opens the pull request. After
its implementation commit and verification, it uses this exact branch name and push procedure:

```bash
cd -- "<workspace>" && \
  jj -R "<workspace>" bookmark set legion/issue-<n> && \
  jj -R "<workspace>" git push --bookmark legion/issue-<n> --allow-new
```

The provisioned issue workspace configures `credential.helper` with the daemon's absolute
credential command, so `jj -R "<workspace>" git push` authenticates transparently through the
same session capability. Never handle a token.

Then create the pull request with the `github` tool's `pr_create` operation. The credential
helper and `legion gh` provide the GitHub identity; never export, fetch, or replace a token.
Other phases advance the existing branch rather than creating a replacement bookmark or PR.

## Completion gate: handoff write, verification, and persistence

The durable handoff uses the phase-specific fields from the task's `outputSchema` only.
`--data` must not include `schemaVersion`, `phase`, or `completed`: the CLI generates that
envelope. Return the **full** schema through the task's structured output, including the
generated envelope fields.

Write the phase-specific handoff:

```bash
cd -- "<workspace>" && \
  legion handoff write --phase <p> --data '<JSON object of phase-specific fields only>'
```

Then verify the durable artifact exists:

```bash
test -f "<workspace>/.legion/<phase>.json"
```

Then commit that exact handoff file onto the issue branch:

```bash
cd -- "<workspace>" && \
  jj -R "<workspace>" split -m "<phase>: record handoff" .legion/<phase>.json
```

If the issue bookmark exists locally, advance it and push it with the provisioned credential
helper. `--allow-new` also publishes the locally provisioned bookmark on its first push:

```bash
cd -- "<workspace>" && \
  jj -R "<workspace>" bookmark set legion/issue-<n> && \
  jj -R "<workspace>" git push --bookmark legion/issue-<n> --allow-new
```

Do not report phase completion until the write, existence check, and handoff commit succeed;
when an issue branch exists, its push is also required. This is the committed copy the next
phase reads after revival. The reviewer later removes `.legion/` as its final commit; phase
workers do not remove it.

## Completion and escalation

Return the same schema as the durable handoff through the task's structured output. Do not add
pipeline labels, run a controller loop, or notify a controller with an invented completion
protocol. A direct worker delivery belongs to its role; overseers receive only derived
verdicts.

When blocked on lifecycle, scope, or cross-phase matters, send the owning architect a concise
hub message: issue, phase, verified observation, what you tried, and the decision required.
Reach for `dispatch` yourself only for a standalone human question outside that coordination.
