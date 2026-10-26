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

The daemon released `<KEY>`'s slot and no waiting root took it: its tree finished or left the
workflow, or the root returned the slot to wait on a person (`awaiting_person`, which also sends
its own `awaiting-person` wake). Name the free slots; take nothing.

## `todo on <KEY>` (`{kind: "todo"}`)

`<KEY>`, an issue nobody handed to Legion, changed while in `todo` and a slot stood free; the
daemon waits half a minute and folds that window's events into one wake. Take nothing: it is not
Legion's.

## `tick on <PROJECT>` (`{kind: "tick", openCapabilities?}`)

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

## `unslotted on <KEY>` (`{kind: "unslotted", reason}`)

Either a root that waited for a slot was moved from `todo` to the status `reason` names, by a
person or by another session, so the daemon took it out of the workflow (`phase: done`); or a root
waiting on a person could not be resumed — its `legion` label is gone, or a person or an outside
session took its Dispatch claim — and the daemon took it out the same way, with its Dispatch
status still `in_progress`. `reason` says which. Re-admit nothing, because the move was someone
else's; `dispatch read` shows who claims it now.
Tell the person in your summary: "<KEY> left Legion's workflow: <reason as the wake gives it>;
`todo` with the `legion` label queues it again". Other
admission states: `skill://legion-controller/references/admission.md`.

## `awaiting-person on <KEY>` (`{kind: "awaiting-person", reason}`)

A root whose architect waits only on a person: the daemon returned its slot and suspends the
architect after its turn. Read `issues.<KEY>.waitingOn`. Do not park it or start anything, and take
nothing for the slot it returned: it is no reason to label or admit an issue ([Free
slots](skill://legion-controller/SKILL.md#free-slots)). `answeredAt` means it is already ahead of
never-run roots and admission resumes it at the next free slot. A missing label or a foreign
Dispatch claim causes the separate `unslotted` wake.

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
