# Wakes

Read this when a wake's row in the skill's table is not enough to act on it. The daemon publishes
these on your controller topic, `notifications.legion.<project>.controller`, only while you are
registered, and sends no second wake of a kind while one is still unsent. Other agents' messages
(READY packets, finding asks, escalations) reach you on your role topic,
`notifications.role.legion-<project>-controller`; you hear both. Check each in `read_state` before
you act, because a wake can be stale.

## `triage on <KEY>` (`{kind: "triage"}`)

An unrecorded root in `triage` that carries the `legion` label. The daemon sends it when the issue
is created with the label, and on each later change while it stays in `triage`, the change that
adds the label included: the dashboard creates an issue without labels, so a person adds it from
the issue header. Triage it as `skill://legion-controller/references/triage.md` says.

## `slot-free on <KEY>` (`{kind: "slot-free"}`)

The daemon released `<KEY>`'s slot, because its tree finished or left the workflow, and no waiting
root took it. Name the free slots; take nothing.

## `todo on <KEY>` (`{kind: "todo"}`)

`<KEY>`, an issue nobody handed to Legion, changed while in `todo` and a slot stood free; the
daemon waits half a minute and folds that window's events into one wake. Take nothing: it is not
Legion's.

## `tick on <PROJECT>` (`{kind: "tick", openCapabilities?, blocked?}`)

The daemon's periodic wake, a minute after it starts and then every
`controller_wake_interval_seconds` (an hour by default), whatever the slots. Run the skill's every-turn
steps.

`openCapabilities`, when present, names the open capability rows; `read_state`'s `capabilities`
gives each one's `detail`. A deployment row (the secrets broker, model fallback, a role's CPU and
memory limits, the pool's capacity at boot) has a `configLine`, the `legion.yaml` line
(`capabilities.decided.<name>`) that would record a decision. A live row (`subagents`,
`web-search`, `mcp`, `repository-extensions`, `dispatch-envoy-tools`, `github`) is a running
session's failed check: its `detail` names the session (pod or pane), role, issue and fact, and it
has no `configLine`. The next daily report names each row still open; take no other action,
because the operator closes or decides a deployment row, and the operator or the issue's architect
fixes a live row's session.

`blocked`, when present, is `[{issue, blockedBy}, …]`: every root and child Legion is waiting to
start behind an unfinished Dispatch issue, each with the issues it waits on. `read_state` shows the
same list under `admission.blocked`, and `legion state` prints it as `blocked on Dispatch links:`.
The wait ends when each blocker reaches `done` (a person's close, or a child's own merge) or its link
is taken off (by a person or the tree's architect): name the waits in the day's report and take
nothing for them.

## `unslotted on <KEY>` (`{kind: "unslotted", reason}`)

A root that waited for a slot was moved from `todo` to the status `reason` names, by a person or by
another session, so the daemon took it out of the workflow (`phase: done`) and starts nothing for
it. Re-admit nothing, because the move was someone else's; `dispatch read` shows who claims it now.
Tell the person in your summary: "<KEY> left Legion's waiting line: it was set to <status> while it
waited for a slot, so Legion let it go; `todo` with the `legion` label queues it again". Other
admission states: `skill://legion-controller/references/admission.md`.

## `held on <KEY>` (`{kind: "held", phase, role?, reason?}`)

Check that the issue's `phase` is `held`. Without `reason`, a phase worker's launches or prompts
ran out, and the tree's architect decides whether to retry or escalate: do nothing. With
`reason: "escalated"` (`issues.<KEY>.holdReason` is `escalated`), the architect handed it to you:
handle it as an escalation (`skill://legion-controller/references/escalation.md`). A held issue
with no `holdReason` while its tree lingers (the time after its root closes when a reopen resumes
it) or is closed waits for the tree's re-admission and needs nothing from you.

When an escalated hold's cause is not gone, park the tree: `set_status` the root to `backlog`. Set
the root back to `todo` only when you checked that the hold's cause is gone, or a person asks,
because `todo` re-admits it as a new generation: a new run of the tree that starts again from its
architect.

## `worker-died on <KEY>` (`role: "architect"`)

The tree's architect ran out of launches or prompts, and the daemon relaunches nothing. Every other
notice of the tree goes to that architect, so nobody inside the tree can act. Check that
`issues.<KEY>.architect.state` is `failed`. When the root's `phase` is `done`, the tree is already
parked: do nothing (a failed architect reads `failed` until the tree closes). Otherwise look for
the cause: an open `capabilities` row whose `detail` names the architect's session, or the
architect's last message on the root issue. When you find none, or it is gone, re-admit the tree
(`set_status` the root to `backlog`, then to `todo`: a new generation whose architect starts with
fresh budgets). When it stays, park the tree (`set_status` the root to `backlog`) and say why on
the issue, because a relaunch would fail the same way.

## GitHub mentions

Read the mention and its artifact. Answer a mention that asks you for triage or for information
that `read_state` or Dispatch holds.
Send any other to the owning architect: find its role in `read_state` and `envoy_publish` the
mention's URL, its text and the issue key you checked. Never build a role token from a partial
issue reference, because a wrong token reaches nobody.

A person who asks how to let a root past its design gate approves the root's spec document in
Dispatch: the `Approve` control in the document's header, or the approval question the architect's
request opened in the Inbox. No operator command opens a gate; a project that wants no gate sets
`gates.design: off` in `legion.yaml`.
