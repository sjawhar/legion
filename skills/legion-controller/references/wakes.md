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

## `agent-question on <KEY>` (`{kind: "agent-question", role, reason, question}`)

Where the deployment configures the daemon's answer loop (`settings.answers` in `read_state`), the
daemon answers a question to an agent that is not running, in the agent's name, from the agent's
own conversation. This wake is a question that answer did not settle, sent once per question:
`question` is `{id, issue, session, asker, ref, text}`, the agent is the `role` of `question.issue`
in `question.session`, and the asker already has a reply in the agent's name saying the question
was passed to you. `reason` says why:

- `not_answered` (the agent could not answer from its conversation and what `gh` reads) or
  `needs_work` (the question asks for work it cannot do while stopped): the agent's own reply;
- `answer_seconds`, `model_calls`, `length`, `refusal`, `malformed` or `provider_error`: the
  answer failed;
- `over_limit` (the project's daily spend), `no_provider`, `no_price`, `unknown_format`,
  `no_conversation` or `post_refused`: the deployment could not answer it or post the reply.

`question.asker` is `user:<login>`, or `session:<id>` with the asking session's title when known.
`question.ref` is where it was asked: `dispatch://<KEY>/message/<id>`, `dispatch://<KEY>/comment/<id>`,
`dispatch://<KEY>/ask/<id>` (a person's answer to the agent's ask that also asks something),
`dispatch message <id>` (the agent's own conversation page), or `envoy:<event id>` (an agent's
Envoy message). `question.text` is the question, cut at 2,000 characters with a note naming where
the whole is.

Read `read_state` and the issue (`dispatch read`), then take the first of these that fits:

- **The issue's tree runs** (its root's `architect.locator.claim` is in `read_state`):
  `envoy_publish` the question, its asker and its `ref` to `notifications.role.` followed by that
  claim, because the tree's architect owns the work.
- **`read_state` or Dispatch holds the answer:** answer the asker where it asked: `dispatch comment
  --issue <KEY> --reply-to <comment id>` for a comment, `dispatch comment --issue <KEY>
  --reply-to-ask <ask id>` for an ask's answer, `dispatch message --issue <KEY> --in-reply-to
  <ref>` for an issue's message, `dispatch message --issue <KEY>` naming the asker for a message on
  the agent's own page, and `envoy_send` to the asking session for an `envoy:` ref.
- **It asks for work on an issue Legion does not run now:** tell the asker the same way that the
  work needs the issue running again, which a person decides (`todo` with the `legion` label), and
  re-admit nothing for it.

A reason the deployment causes (`over_limit`, `no_provider`, `no_price`, `unknown_format`,
`no_conversation`, `post_refused`) also goes in the next daily report, because the operator's
`answers` configuration fixes it. Act on a question once: one you already answered, by your reply
on its `ref` thread or your message to the asker, needs nothing, whether it reaches you as this
wake or in `questionsForController`. An entry of `questionsForController` carries no `text`: read
the question at its `dispatch://` ref, and for an `envoy:` ref ask the asking session to send it
again.

## `agent-decision on <KEY>` (`{kind: "agent-decision", role, agent_decision}`)

Where the deployment configures the daemon's answer loop, a person's decision on an ask that an
agent that is not running opened (their answer to the ask, or their reply that hands the ask back
to the agent) goes once to the architect that owns the agent's issue while that architect holds
its role, and to you otherwise. `agent_decision` is `{id, issue, session, asker, ref, text}`: the
agent is the `role` of `agent_decision.issue` in `agent_decision.session`, `asker` is the person
(`user:<login>`), `ref` is their answer (`dispatch://<KEY>/ask/<id>`) or their reply
(`dispatch://<KEY>/comment/<id>`), and `text` is the options they selected and what they wrote,
cut at 2,000 characters with a note naming where the whole is. The daemon already answered any
question the same message asks, in the agent's name; one that answer did not settle reaches you as
an `agent-question` wake of its own.

Read `read_state` and the `ref`, then take the first of these that fits:

- **The issue's tree runs** (its root's `architect.locator.claim` is in `read_state`):
  `envoy_publish` the decision, its `asker` and its `ref` to `notifications.role.` followed by
  that claim, because the tree's architect acts on it.
- **The tree is parked or finished:** a decision does not by itself re-admit a tree a person
  parked, because only a request or a plan ruling that names the issue moves it out of `backlog`
  or `icebox` (`skill://legion-controller`, "Free slots"). Re-admit nothing for it. When the
  decision needs the tree to run, tell the person on its `ref` thread (`dispatch comment --issue
  <KEY> --reply-to <comment id>`, or `--reply-to-ask <ask id>` for an answer) that it does, which
  they decide (`todo` with the `legion` label), and name the decision in your summary.

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

A person who asks how to let a root past its design gate approves the root's spec document in
Dispatch: the `Approve` control in the document's header, or the approval question the architect's
request opened in the Inbox. No operator command opens a gate; a project that wants no gate sets
`gates.design: off` in `legion.yaml`.
