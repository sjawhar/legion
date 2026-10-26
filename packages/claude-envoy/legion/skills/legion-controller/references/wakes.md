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

## `tick on <PROJECT>` (`{kind: "tick", openCapabilities?}`)

The daemon's periodic wake, a minute after it starts and then every
`controller_wake_interval_seconds` (an hour by default), whatever the slots. Run the skill's every-turn
steps, then the parked-root walk (`skill://legion-controller/references/parked-decisions.md#the-walk`).

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

A root that waited for a slot was moved from `todo` to the status `reason` names, by a person or by
another session, so the daemon took it out of the workflow (`phase: done`) and starts nothing for
it. Re-admit nothing, because the move was someone else's; `dispatch read` shows who claims it now.
Tell the person in your summary: "<KEY> left Legion's waiting line: it was set to <status> while it
waited for a slot, so Legion let it go; `todo` with the `legion` label queues it again". Other
admission states: `skill://legion-controller/references/admission.md`.

## `awaiting-person on <KEY>` (`{kind: "awaiting-person", reason}`)

A root whose architect waits only on a person (a closed design gate, or its question on the root),
so the daemon returned its slot and suspended the architect's session after its turn. Read
`issues.<KEY>.waitingOn`: the gate version or the ask it waits on. Do not park it or start
anything, and take nothing for the slot it returned: it is no reason to label or admit an issue
(the skill's free slots). `answeredAt` means a person answered: the root is already ahead of
never-run roots, and admission resumes it at the next free slot. A missing label or a foreign
Dispatch claim causes the separate `unslotted` wake. Admission states:
`skill://legion-controller/references/admission.md`.

## `parked-decision on <KEY>` (`{kind: "parked-decision", decision, root, ref, actor, version?, reason?}`)

A person's spec decision, or answer, resolution or reply on an ask, on a tree Legion is not
running, which nobody inside the tree heard. Decide whether `root` runs again as
`skill://legion-controller/references/parked-decisions.md#deciding` says. When the decision gives
its work a next step, `set_status` the root to `todo` only when the park is your own hold (its step
2: by `session legion-daemon:<PROJECT>`, with your `Parked by Legion while it waits on:` mark beside
it), never for any other park, an operator's `legion status` included (then leave it parked).
Either way, post the `Parked decision read:` comment its step 4 gives (when the root's phase is not
`done`: `backlog`, then `todo`, only if its `architect.state` is `failed` or `retired`, and nothing
while its architect lives). When it only questions the work, leave the root as it is and answer on
the question's ask (a request for changes: on the issue whose spec it reviews).

## `held on <KEY>` (`{kind: "held", phase, role?, reason?}`)

Check that the issue's `phase` is `held`. Without `reason`, a phase worker's launches or prompts
ran out, and the tree's architect decides whether to retry or escalate: do nothing. With
`reason: "escalated"` (`issues.<KEY>.holdReason` is `escalated`), the architect handed it to you:
handle it as an escalation (`skill://legion-controller/references/escalation.md`). A held issue
with no `holdReason` while its tree lingers (the time after its root closes when a reopen resumes
it) or is closed waits for the tree's re-admission and needs nothing from you.

When an escalated hold's cause is not gone, park the tree: `set_status` the root to `backlog`,
then, in the same turn, post one comment on the root (`dispatch comment --issue <root>
--body-file -`) whose first line is `Parked by Legion while it waits on: <the hold's cause>`, the
cause being what you found the held phase waits on (the payload's `phase` and `role`, and the held
issue's `dispatch read`). That mark is what makes the park Legion's own hold
(`skill://legion-controller/references/parked-decisions.md#deciding`, step 2). Set the root back to
`todo` only when you checked that the hold's cause is gone, a person asks, or a person's decision
after the park gives its work a next step (that reference's step 4), because `todo` re-admits it
as a new generation: a new run of the tree that starts again from its architect.

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
