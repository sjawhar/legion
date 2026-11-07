# Wakes

Read this when a wake's row in the skill's table is not enough to act on it. The daemon publishes
these on your controller topic, `notifications.legion.<project>.controller`, only while you are
registered, and sends no second wake of a kind while one is still unsent. Other agents' messages
(READY packets, finding asks, escalations) reach you on your role topic,
`notifications.role.legion-<project>-controller`; you hear both. Check each in `read_state` before
you act, because a wake can be stale.

## `triage on <KEY>` (`{kind: "triage"}`)

An unrecorded root in `triage` that carries the `legion` label, when no person set that label (a
session's label, yours included). The daemon sends it when a session creates the issue with the
label, and on each later change while it stays in `triage`, the change that adds the label
included. Triage the root as `skill://legion-controller/references/triage.md` says. A person's
label on a `triage` root wakes nobody: the root is queued by the daemon (its own `todo` write). A
person's approval of a parked (`backlog` or `icebox`) labelled root's spec is queued by the daemon
the same way and wakes nobody.

## `slot-free on <KEY>` (`{kind: "slot-free"}`)

The daemon released the slot of `<KEY>` and no waiting issue took it: the work of that issue ended
or left the workflow — its merge, `accepting`, a blocker, its own children, a person
(`awaiting_person`, which also sends its own `awaiting-person` wake), or its tree finishing. A
slot is one issue with work, a root or a child, so the issue it names may be either. Name the free
slots; take nothing.

## `todo on <KEY>` (`{kind: "todo"}`)

`<KEY>`, an issue nobody handed to Legion, changed while in `todo` and a slot stood free; the
daemon waits half a minute and folds that window's events into one wake. Take nothing: it is not
Legion's.

## `tick on <PROJECT>` (`{kind: "tick", openCapabilities?, blocked?}`)

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

`blocked`, when present, is `[{issue, blockedBy}, …]`: every root and child Legion is waiting to
start, each with what it waits on — the unfinished Dispatch issues of its `blocked_by` links, or,
for a parent, the open children beneath it that children-first holds it for. `read_state` shows the
same list under `admission.blocked`, and `legion state` prints it as `blocked on Dispatch links:`.
A link's wait ends when each blocker reaches `done` (a person's close, or the blocker's own merge) or its
link is taken off (by a person or the tree's architect), and a children-first wait ends when the
last open child leaves the workflow: name the waits in the day's report and
take nothing for them.

## `unslotted on <KEY>` (`{kind: "unslotted", reason}`)

Either a root that waited for a slot was moved from `todo` to the status `reason` names, by a
person or by another session, or a root waiting on a person (`awaiting_person`) could not be
resumed — its `legion` label is gone, or a person or a session outside its tree took its Dispatch
claim. Either way the daemon took it out of the workflow (`phase: done`) and starts nothing for it;
`reason` says which. Re-admit nothing, because the move was someone else's; `dispatch read` shows
who claims it now. Tell the person in your summary: "<KEY> left Legion's workflow: <reason as the
wake gives it>; `todo` with the `legion` label queues it again". Other admission states:
`skill://legion-controller/references/admission.md`.

## `awaiting-person on <KEY>` (`{kind: "awaiting-person", reason}`)

A root whose architect asked a person a question, or handed its spec to a person for approval, and
now waits only on them: the daemon returned its slot while the architect stays live to read the
person's comments and replies. Read `issues.<KEY>.waitingOn`. Do not park it or start anything, and
take nothing for the slot it returned: it is no reason to label or admit an issue ([Free
slots](skill://legion-controller/SKILL.md#free-slots)). The person's answer puts it among the
issues at the front of `admission.waiting` by itself, and its promotion gives its architect the
answer task: at once, holding no slot, when its design gate has already opened, and at the next
free slot while its architect is still writing the first spec. A lost label or a foreign
Dispatch claim sends the separate `unslotted` wake.

## `parked-decision on <KEY>` (`{kind: "parked-decision", decision, root, ref, actor, version?, reason?}`)

A person's spec decision, or answer, resolution or reply on an ask, on a tree Legion is not
running, which nobody inside the tree heard. The payload names the decision, the tree's `root`,
the decision's Dispatch `ref`, the person (`actor`), and a spec decision's `version` and a changes
request's `reason`. Decide it as `skill://legion-controller/references/parked-decisions.md#deciding`
says.

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

## `worker-stalled on <KEY>` (`{kind: "worker-stalled", role: "architect", reason}`)

The daemon's stall backstop ended a turn of the tree's architect: its agent had not stepped for
longer than the `worker_stall_backstop_hours` the deployment gives that role, so the daemon ended
the turn and brought the same session back on a new process. You get a copy only for a tree's own
architect, because every other notice of the tree goes to that architect and nobody inside the
tree is left to read this one; the architect is told on its issue as well. The `reason` is the
record the daemon read: when the agent last stepped and how long ago that was, the bound it
passed, when the turn began, and the last call the daemon saw, with its tool, its call id, when it
started and whether it ended — never a call's arguments, whose text the daemon keeps in no column.

Nothing failed and nothing was charged to the claim: the task that architect was serving is still
owed to it and is sent to it again, no issue was held, and no status moved. Do nothing to the
tree — park nothing, re-admit nothing, set no status — and start nothing. One copy comes per
stall, so an architect that stalls again and again sends one each time: that is a finding to put
to a person ([Findings](skill://legion-controller/SKILL.md#findings-and-the-issues-you-file)),
with the records you were sent in it.

## `update-stalled on <KEY>` (`{kind: "update-stalled", reason, update}`)

An update round over the issue's pod, or over one claim's own process, is not finishing. Handle
it as `skill://legion-controller/references/escalation.md#an-update-round-that-is-not-finishing-update-stalled`
says.

## GitHub mentions

Read the mention and its artifact. Answer a mention that asks you for triage or for information
that `read_state` or Dispatch holds.
Send any other to the owning architect: find its role in `read_state` and `envoy_publish` the
mention's URL, its text and the issue key you checked. Never build a role token from a partial
issue reference, because a wrong token reaches nobody.

A person who asks how to let a root past its design gate approves, in Dispatch, the document of
each step the deployment gates, which your system prompt's `Design gate steps:` sentence names:
the `Approve` control in that document's header, or the approval question the architect's request
opened in the Inbox. The root's work starts when every one of those steps is approved. No
operator command opens a gate; a project that wants no gate sets `gates.design` to the empty list
(`design: []`) in `legion.yaml`.
