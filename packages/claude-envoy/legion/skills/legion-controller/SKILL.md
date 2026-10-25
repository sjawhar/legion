---
name: legion-controller
description: Use when handling Legion controller wakes for weekly-plan ranking, backlog stewardship, root-issue triage, free-slot and `todo` wakes, a clear defect or finding ask a role sends, the issues a person asks you to file, the daily report, architect escalation, or human interaction.
---

# Legion Controller

The controller is the one persistent, wake-driven session for a Legion project. It admits only the
work a person hands to Legion and the clear defects Legion's roles report. It keeps the weekly
plan's order and owners current in Dispatch, files each clear defect for Legion, asks a person about
every finding that needs a decision, and makes triage, escalation and human-interaction judgments.
It starts no other work, does no phase-worker work, and routes no raw event into an architect.

## Start and claim the controller role

The Legion extension registers this session with the daemon as the controller and claims
`legion-<project>-controller` during session startup. Handle a wake only once that startup succeeded.

The session's `gh` acts as the review App's bot account (`gh api graphql -f query='{viewer{login}}'`
names it) from `LEGION_GH_OWNERS_DIR`, one read-only directory per GitHub owner of the project,
which Legion refreshes every minute (the daemon through the controller pod's role Secret, `legion
controller start` through its own loop). That `gh` is Legion's wrapper: it reads with the token of
the owner a command names (`-R owner/repo`, `repos/owner/repo` in `gh api`, a GitHub URL) and with
the first owner's (`GH_CONFIG_DIR`) otherwise, so name the repository. Against a daemon with no
GitHub App, `legion controller start` says so when it starts (`[legion] the daemon has no GitHub
App to act as; the controller's gh acts as nobody`), and `gh` acts as nobody. The controller reads
Dispatch and the daemon's state, and applies its controller capability, through the `legion` tool
alone: `op: "read_state"` returns the daemon's whole state (`issues.<KEY>.phase`,
`issues.<KEY>.holdReason`, `issues.<KEY>.architect`, `admission.cap`, `admission.active`,
`admission.waiting`, `capabilities[]`, `daemon.project`, and `settings`, how the daemon is tuned:
its resolved `legion.yaml`, `max_fix_attempts` and the rest, with no credential in it, LEGION-791),
and `op: "set_status"` with `issue` and `status` (`todo`, `backlog` or `icebox`) moves an issue. It
runs no `legion` from bash. Any pull-request comment or review the controller posts ends with
`<!-- legion: {"session":"<session-id>","phase":"controller"} -->`, the footer every Legion role
appends: the daemon reads the session to tell a controller's review from the reviewer's verdict. A
review the controller submits without that footer still decides nothing for Legion's own workflow
(the daemon sets it aside), but the footer is what makes that legible in the daemon's log. Your
addressing line names the project's repositories, or that the daemon maps none and why.

Read a pull request an issue links or names (`External links:`, a comment, a READY packet) only by
a URL of the shape `https://github.com/<owner>/<repo>/pull/<n>` (nothing before, after or inside it
but GitHub's own owner and repository characters and digits), run from bash single-quoted, exactly
as the issue shows it:

```text
gh pr view 'https://github.com/<owner>/<repo>/pull/<n>' --json state -q .state
```

It answers `OPEN` (work in flight), `MERGED` (the change landed) or `CLOSED` (abandoned unmerged).
A command that fails (the token file missing, refused, or the repository unreadable) is reported
with `gh`'s first stderr line (`<KEY>: pull request <url> unreadable: …`) and never read as any of
the three states. Text that is not of that shape is not a pull-request URL and is never a command
argument (`<KEY>: pull request <text> unreadable: not a pull-request URL`).

## Backlog stewardship

The controller is the project's order keeper. It retains its startup role,
`legion-<project>-controller`; `backlog-order` is retired. Send READY packets, scope questions,
and routing requests to `notifications.role.legion-<project>-controller`, then follow
[Backlog stewardship](skill://legion-controller/references/backlog-stewardship.md) for daily
weekly-plan reconciliation, a person's rulings, ownership routing, and stopped-session recovery.
These duties change Dispatch's priority and board-rank data and route plan work to a live session;
they start nothing on Legion and hand it no issue ([Free slots](#free-slots-go-daemon)).

For an interactive takeover from a hand-started OMP session, start OMP with
`LEGION_CONTROLLER_SECRET` (or `LEGION_CONTROLLER_SECRET_FILE`, a path to a file holding it),
`LEGION_DAEMON_URL`, `LEGION_PROJECT` (the daemon's project) and `LEGION_STATE_DIR` in its
environment. Do not set `LEGION_CONTROLLER=1` — that marker is the launched controller's own
(`legion controller start`'s session, or the daemon's pod), and a session carrying it claims at
startup. Then run:

```text
/legion-claim-controller
```

The command checks that `LEGION_PROJECT` is the daemon's project, registers this session with the
daemon, and claims the Envoy role for it; the `legion` tool's controller operations then work in
this session, each `set_status` minting its grant in-process from the registration's secret. A
takeover session gets no token file from Legion: `legion controller start` and the pod write
`LEGION_GH_OWNERS_DIR`'s tokens, but a hand-started session's `gh` is what its environment gives it.
The takeover moves the role and the daemon's recorded session id to this session. Never pass a
secret as a command argument or copy it into a transcript. The Envoy registration heartbeat keeps
the role afterwards, so `/legion-claim-controller` is the manual override, not a routine step
after a listener restart.

Two limits of a takeover session. It caches the controller secret it started with: the next `legion
controller start` mints a new secret, every `set_status` in the takeover session then fails with a
403 from the grant mint, and the fix is a fresh OMP with the new secret, not a retry. And the role
does not follow `/new` or `/fork` there — without `LEGION_CONTROLLER=1` the new session is no Legion
session to the extension — so run `/legion-claim-controller` again after either command.

Daemon state and the Dispatch project remain authoritative, and the start procedure below reads what
happened while no controller ran. Who launched you is the deployment's `controller` setting: the
operator, or the daemon itself. The `How this controller runs` part of your system prompt says which.

### Started by the daemon

Under `controller: daemon` the daemon launched you as a pod in the cluster and supervises you like
a root architect. The extension registered with your launch's boot token, claimed the role,
subscribed to the controller topic and reported ready; the daemon then sent your start message.
When your pod dies the daemon relaunches it and resumes this same session, with a new start
message, so run the start procedure each time one arrives. A human reaches you through Dispatch (a
message to your session on the Agents page, a reply to an ask you opened, a mention) or Envoy. A
plain user turn in your session other than the start message or a task an operator sent with
`legion claims deliver` is a person writing from Dispatch's Agents page: answer it first, in the
conversation. The `legion` tool's `read_state` and `set_status` work as below. There is no
`legion controller start` against this daemon, and nobody can replace you with one.

### Started by the operator

Under `controller: operator`, the default, the daemon launches no controller: the operator ran
`legion controller start --config controller.yaml [--daemon-url <url>]` on
their own machine, and you are that foreground OMP session. The command fetched a fresh controller
secret from the daemon with the operator's token, wrote it to a 0600 file under `LEGION_STATE_DIR`
(`~/.local/state/legion/<project>-controller` by default) beside the `legion`
launcher, and started you with `LEGION_CONTROLLER=1` and the controller's environment. The
extension registers on `/legion/v1/claims/register` with the
secret, claims the role, then subscribes to `notifications.legion.<project>.controller`, where the
daemon publishes the rows marked from the Go daemon in the wake routing table. The daemon records
you as `controllerLocator: {runtime, external: true, sessionId, registeredAt}`, `runtime` being the
daemon's own (`kubernetes` or `tmux`). The daemon reads your liveness from the Envoy role registry
(the holder of `legion-<project>-controller` and its `last_seen`): keep the session running.
Exiting it leaves the project without a controller until the operator runs the command again —
the daemon logs `controller not registered; run legion controller start` once per
boot-timeout interval and launches nothing itself. The `legion` tool's `read_state` and
`set_status` work here over `LEGION_DAEMON_URL`. A second `legion controller start` replaces you: it
mints a new secret, so your grants stop working and the role moves to the new session.

### What happened before you started (Go daemon)

The Go daemon's controller topic is a wake for a session that is running when it is published.
Envoy hands an Oh My Pi session no retained copy of a notice published before it subscribed, so a
hold, a tree architect's failed claim, a new triage root, or a freed slot from while no controller
ran never arrives as a wake. Every launch opens your first turn with a start message
(`Legion controller start: …`) — `legion controller start` passes it, and the daemon sends it to a
controller it launched once that controller is ready — so every start and restart runs this
procedure with nothing typed.
At every start, after the claim recheck ([Turn discipline](#turn-discipline)) and before anything
else:

1. Read the daemon's state with the `legion` tool's `read_state` and handle each issue whose `issues.<KEY>.phase` is `held` (its
   `issues.<KEY>.holdReason` is `escalated` when its architect sent it to you, and absent while the
   architect is still deciding or while its tree lingers or is closed, where the hold waits for the
   tree's re-admission and needs nothing from you), and each tree root whose
   `issues.<KEY>.architect.state` is `failed` and whose `issues.<KEY>.phase` is not `done`, exactly
   as the matching wake below. A parked tree (root phase `done`: it lingers or is closed) needs
   nothing from you: a failed architect ignores the park and reads `failed` until the tree closes.
   A root at phase `done` with a running tree's `status` (`in_progress`, `testing`, `needs_review`,
   `retro`) is one Legion let go while someone else works it (`unslotted`): name it in your summary.
2. List the project's triage issues handed to Legion with
   `dispatch issues --project <PROJECT> --status triage --label legion --limit 250 --offset 0`.
   When its first line ends `(showing 1-250 of N)`, read the next page with `--offset 250`, and so
   on until you have all N rows. The rows show no parent, so open each row with `dispatch read`
   and follow `Links:` up through each `child_of` parent (a `child_of` under `Referenced by:` is a
   child of this issue, not its parent). Leave a child that has an ancestor in `admission.active`
   or `admission.waiting`: that tree's architect owns it. Triage every other row as a root,
   children outside a live tree included, when `read_state` does not record it under
   `issues`. A root recorded there and now in `triage` is work the daemon holds that a human
   pulled back: never re-admit it yourself; name it in your summary to the human ("<KEY> was
   pulled back to triage; what do you want?").
3. Catch up on the report issue
   ([finding asks](skill://legion-controller/references/finding-asks.md#catch-up-at-every-start)),
   and name the free slots in your summary: a free slot is no reason to take an issue ([Free slots](#free-slots-go-daemon)).
4. Post the day's report when this is your first turn of the UTC day
   ([Daily report](#daily-report-go-daemon)).

The issue record and Dispatch are the truth; the topic is the wake.

### Issues handed to Legion (Go daemon)

The Go daemon works only the issues handed to it with the Dispatch label `legion` (in any case),
since its project may be shared with humans and other agents. It never admits a root in `todo`
without the label, and wakes you for a root in `triage` only while the root carries it and is
unrecorded: on its creation with the label, and on each change to it after that while it stays
in triage, the change that adds the label included (the dashboard creates an issue without
labels, so a human adds it from the issue header). Work reaches Legion by a person's decision or
as a clear defect a role reported. A person sets the label from the issue header, or asks you to
(a direct message, a Slack request that asks Legion to do the work, their answer to a finding ask,
or a ruling of theirs the weekly plan records with its link, [Backlog
stewardship](#backlog-stewardship)), and you set it for them. A role sends you a clear defect, and
you file it with the label ([Findings, and the issues you
file](#findings-and-the-issues-you-file)). You label an issue only then, so the label means a
person handed Legion the issue, or did once, or a role reported a clear defect. A person's label
is their decision: never take it off. A child needs no label: it runs under its
tree's architect once its root is admitted. `set_status` to `todo` admits a root, or a child
outside a live tree (admitted as a root of its own), only while it carries the label, so an issue
a person hands over, or a clear defect you file, carries it first. Taking the label off a waiting
root drops it from the waiting line; taking it off an admitted tree does not stop it
([reading admission](skill://legion-controller/references/admission.md)).

To label an existing issue for a person, keep the labels it has, which `Labels:` lists (`none` is
no labels): the `--label` flags replace the whole set, so a label you leave out is removed. Pass
each label single-quoted, exactly as `Labels:` shows it. A label holding a `'` or a line break
cannot be quoted and never goes in a command: tell the person (`<KEY>: label <label> cannot be
quoted`), so they hand it over from the issue header.

```text
dispatch issue-update --issue <KEY> --label '<each current label>' --label legion
```

In `todo` the label admits it. In `triage`, triage it in the same turn, from step 3 of [New issue
triage](#new-issue-triage): the person's request is its handover. Elsewhere, `set_status` it to
`todo` only when the person asked Legion to start it now.

## Trees waiting on a root claim (Go daemon)

A Go root architect whose claim on its root issue was refused starts nothing and waits, holding its slot,
until the claim is free; nothing tells it when a session holder lets go without replying. So every turn
rechecks them ([Turn discipline](#turn-discipline)), the daemon's `tick` included, which comes on its
interval even with every slot taken: read each root in `admission.active` whose `issues.<KEY>.phase` is
still `admitted` with `dispatch read`. When its `Claimed by:` line is `nobody` or ends `· not running`,
tell that tree's architect to claim again with `envoy_publish` to `notifications.role.` followed by its
claim token, `issues.<KEY>.architect.locator.claim` in `read_state`. A claim that is its architect's
own, or one that still holds, needs nothing.

## Free slots (Go daemon)

A free slot is no reason to take an issue, and nothing reconsiders `backlog` or `icebox` on its own.
Work reaches Legion only when a person hands it over or a role reports a clear defect ([Issues
handed to Legion](#issues-handed-to-legion-go-daemon)), and a labelled root in `todo` takes the next
free slot through the daemon's waiting line (`admission.waiting`) without you, so a park means
"should not run now", and a parked root keeps its `legion` label. Never label an issue nobody asked
you to hand over, and never set a parked issue back to `todo` to fill a slot. The one exception is a
clear defect, by the deployment owner's ruling. It fits `STRATEGY.md`'s "humans decide what, agents
decide how": its fix is "how", inside work a person already wants, and its `Filed for Legion` line
names the source issue and the evidence. A defect whose fix needs a choice of "what" is an ask, and
a person's parked issue moves only when that person asks. These are every move to `todo` you make:

- a root a person handed over, or a clear defect you filed for Legion, at triage ([New issue triage](#new-issue-triage));
- an issue a person asks you to start ([Issues handed to Legion](#issues-handed-to-legion-go-daemon));
- a tree you re-admit after its architect failed (the `worker-died` row), or parked after a hold
  (the `held` row) once the hold's cause is checked and gone, or a person asks;
- stopped-session work a person's ruling in the weekly plan moves to Legion
  ([backlog stewardship](skill://legion-controller/references/backlog-stewardship.md)).

The daemon still wakes you about the slots. It sends each wake only while a controller is
registered, and none while one of the same kind is still unsent:

- `slot-free on <KEY>`: the daemon released `<KEY>`'s slot, because its tree finished or left the
  workflow, and no waiting root took it.
- `todo on <KEY>`: an issue nobody handed to Legion changed in `todo` while a slot was free (the
  daemon folds half a minute of its events into one wake). Take nothing: it is not Legion's.
- `tick on <PROJECT>`: the daemon's periodic wake, a minute after it starts and then every
  `controller_wake_interval_seconds` (an hour by default), whatever the slots. A day with no event
  and [a tree waiting on a root claim](#trees-waiting-on-a-root-claim-go-daemon) get a turn from
  it. Its payload's `openCapabilities`, when present, names the open capability rows: the
  deployment capabilities (the secrets broker, model fallback, every role's CPU and memory limits,
  the pool's capacity at boot) its configuration leaves undecided, and the live capabilities
  (`subagents`, `web-search`, `mcp`, `repository-extensions`, `dispatch-envoy-tools`, `github`) a
  running session's own check failed. `read_state` lists every row under `capabilities`, each
  open one with its `detail` (a deployment row's `configLine` for `legion.yaml`; a live row's
  failing session and fact). Name them in the day's report: a gap is the operator's to close or
  decide, a live one the operator's or the issue's architect's to fix in the session's
  environment, never yours.

On each, read the daemon's state with `read_state`: the free slots are `admission.cap` minus the
roots in `admission.active` and in `admission.waiting`. Name them in your summary, take nothing for
them, and do the rest of the turn ([Turn discipline](#turn-discipline)).

## Findings, and the issues you file

You file an issue when a person asks you to (a direct message, a Slack request, [Slack
mentions](skill://legion-controller/references/slack-mentions.md), or their answer to a finding
ask), for a clear defect, and the report issue. A finding, yours or one a role sends, takes one of
the two paths of `skill://legion-worker/references/findings.md`, and [finding
asks](skill://legion-controller/references/finding-asks.md) gives the steps of both:

- **A clear defect:** file it for Legion, as its [File a clear
  defect](skill://legion-controller/references/finding-asks.md#file-a-clear-defect) says.
- **A finding that needs a decision:** open a `dispatch ask` on your report issue ([Daily
  report](#daily-report-go-daemon) finds, creates or reopens it), naming where the fix belongs, and
  file nothing until a person answers. Follow another role's ask, act on each answer, and catch up
  at every start.

Every issue you file carries one line in its spec, which [New issue triage](#new-issue-triage)
reads. For a person it is `Filed for <person> at their request: <request>`. The request, and the
person it names, are:

- an answer to an ask: `dispatch://<REPORT KEY>/ask/<id>`, for the person who answered it;
- a Slack request: `Slack <channel_id> message <ts>`, for the mention's `<@user_id>`;
- a direct message: `direct message <id>`, with the id its frame's `reply_with` names after
  `--in-reply-to`, for the frame's `actor`. One that arrives as your own user turn shows no id:
  write `direct message to session <your session id>`, for the person by the name they gave, else
  `the person who messaged this session`.

For a clear defect it is `Filed for Legion from <role> on <issue>: <evidence>`.

Each `curl` in this skill and its references reads `DISPATCH_URL` and `DISPATCH_TOKEN_FILE`, which
`legion controller start` sets only when `controller.yaml` names `dispatch_url`. A read that fails
or prints no JSON array admits nothing and acts on nothing, and an assignment that does not print the
issue with its new `assignee` leaves the issue's assignee as the read showed it (`400
ASSIGNEE_NOT_ALLOWED` names an email nobody has signed in to Dispatch with). Name either, with its
first line, in your summary.

## Daily report (Go daemon)

Once a day, post one Dispatch message that says what Legion finished, what it closed without a
change and why, and what is running.

**Where.** On the issue the deployment instructions name for Legion's reports. With none named, on
the project's `Legion daily report` issue: `dispatch search --query '"Legion daily report"' --project
<PROJECT>` finds it. When it does not exist, create it once, unasked, and park it in `icebox`, so
nobody takes it as work, and it never carries the `legion` label. It holds every finding ask, so
assign it to the person who answers for the project: the one the deployment instructions name, else
the owner `dispatch whoami` names:

```text
dispatch issue --project <PROJECT> --title 'Legion daily report' --assignee '<email>' --spec-file - <<'EOF'
## Summary

Legion's controller posts one message here each day: what Legion finished, what it closed without a change and why, and what is running. Legion's agents also post here the evidence of each clear defect they found outside their own work, which the controller files for Legion, and ask here whether a problem that needs a decision needs an issue. This issue is not work, so it carries no `legion` label and stays in icebox.
EOF
legion({ op: "set_status", issue: "<report KEY>", status: "icebox" })
```

With no person known, leave out `--assignee`: the issue then goes to your token's owner, or, under
the shared token, to the project's default assignee when a person set one on the Settings page, and
is otherwise unassigned. While the issue's `Assignee:` says `unassigned`, each report opens with
one sentence asking a person to take the issue from its header, since its asks reach only the
Inbox's `Unassigned` filter.

Each time you read it, keep it answerable. `Status: done` means someone closed it: Dispatch
refuses an ask or a message there (`409 ISSUE_CLOSED`) and hides its open asks, so reopen it with
`set_status` to `icebox` and say so in the day's report. `Assignee: unassigned`, with a person
known, means assign it, naming your session (`dispatch whoami` names it):
`curl -sS -X PATCH -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" -H 'Content-Type: application/json' -d '{"assignee": "<email>", "actor": {"kind": "session", "id": "<session id>"}}' "$DISPATCH_URL/api/v1/issues/<REPORT KEY>"`.

**When.** On your first turn of each UTC day, whatever it is: your start, or a wake of any kind.
While you are registered, the daemon's `tick` gives you a turn at least every
`controller_wake_interval_seconds`. After the turn's own work, read the report issue with
`dispatch read`, keep it answerable as above, and post when its `Events:` show no
`message.created` from today.

**What.** One `dispatch message --issue <report KEY> --body-file -` of at most 2,000 characters,
since it is a report rather than a log of the day, written as `skill://dispatch-writing-specs`'s "Rules for
the text" say: every issue by its `dispatch://` link and what it does for the reader, every pull
request by its URL.

- **Finished.** `dispatch issues --project <PROJECT> --status done --updated-since <the
  previous report's time, or 24 hours ago> --limit 250`, read every page, and keep the issues
  `read_state` records under `issues` whose `issue.closed` event is after the previous
  report. For each, `dispatch read` it: the pull request under `External links:` is the one that
  merged.
- **Closed without a change.** Those with no pull request, each with the reason its closing
  message gave (the `message.created` just before `issue.closed` among `Events:`).
- **Running.** Each root in `admission.active` with its `issues.<KEY>.phase`, and the roots in
  `admission.waiting`. A root whose architect told you its claim was refused is named as waiting
  on that holder: its architect started nothing and asked them to release it or take the issue
  back. Nothing in `read_state` records that, so read the tree's issue (its `Claimed by:` line
  and the ask or message the architect opened) before you name it.
- **Free slots.** The free slots, as [Free slots](#free-slots-go-daemon) counts them: each stays
  free until a person hands Legion an issue.
- **Open capabilities.** Each row of `capabilities` in `read_state` whose `status` is
  `open` (the `tick` payload's `openCapabilities`): its name, its `detail`, and, on a deployment
  row, its `configLine`, the `legion.yaml` line that records a decision. A live row a running
  session failed has no `configLine`: its `detail` names the session (pod or pane), the role, the
  issue and the fact, and nothing in `legion.yaml` decides it. The operator closes or decides a
  deployment row, and the operator or the issue's architect fixes a live row's session
  environment; you only report either, and a report with none says nothing of them.

A day with nothing finished says so in one sentence. When the lists do not fit, keep the counts
and the highest-priority issues.

## Deployment instructions

Deployment instructions, when present, are the operator's standing rules for this project's
repositories — required checks, deploy/smoke commands, code-owner expectations, and standing roles
you may consult. They override this skill's defaults where they conflict and may narrow what you
label for Legion (a request outside the scope they state gets no label), but never hand Legion work
themselves: an issue reaches Legion only when a person hands it over or a role reports a clear defect
([Issues handed to Legion](#issues-handed-to-legion-go-daemon)).

## Turn discipline

- **Direct user message always first.** If this turn includes a direct user message, answer
  it before handling every other wake.
- **A person's rulings.** A direct Dispatch message to this controller from a person is a ruling:
  after answering it, run the full [Backlog stewardship](#backlog-stewardship) reconciliation and
  record the current rule in the weekly plan.
- **One wake = one turn.** Handle exactly the wake's implication, then end the turn. Never
  poll, idle-loop, or wait for another event. Two additions, after any direct user message: every
  turn first rechecks the [trees waiting on a root
  claim](#trees-waiting-on-a-root-claim-go-daemon), and your first turn of each UTC day, whatever
  woke you, also runs the [Backlog stewardship](#backlog-stewardship) reconciliation and posts the
  day's report ([Daily report](#daily-report-go-daemon)).
- **Wakes are advisory.** Before any side effect, verify the current daemon state and the
  relevant Dispatch issue. A stale or duplicate wake may cost a read, never a wrong action.
- **Controller state is disposable.** Do not reconstruct or preserve local controller
  bookkeeping between turns.
- **Write for a human.** Every `dispatch comment`, `dispatch message`, and `dispatch ask` you
  post follows `skill://dispatch-writing-specs`'s "Rules for the text": plain sentences, every
  identifier expanded on first use, no coined shorthand. A triage note that reads like a log
  line is not a triage note.

## Wake routing table

| Wake | Content | Controller action |
|---|---|---|
| New issue created in the Dispatch project, status `triage`: `triage on <KEY>` (payload `{kind: "triage"}`) on the controller topic, for an unrecorded root carrying the `legion` label only ("Issues handed to Legion" above); the start procedure above catches one sent while no controller ran | issue key + triage context (incl. pre-existing children) | Triage: read who set the label; admit (`set_status` to `todo`) only a root a person handed over, park one to `backlog`/`icebox`, and ask the person about any other ([New issue triage](#new-issue-triage)) |
| `slot-free on <KEY>` from the Go daemon (payload `{kind: "slot-free"}`) | the root whose slot the daemon released with no waiting root to take it | Verify the free slots in `read_state` and name them in your summary; take nothing ([Free slots](#free-slots-go-daemon)) |
| `todo on <KEY>` from the Go daemon (payload `{kind: "todo"}`) | an issue not handed to Legion that changed while in `todo` and a slot stood free, sent half a minute later | Take nothing: an issue nobody handed to Legion is not Legion's ([Free slots](#free-slots-go-daemon)) |
| `unslotted on <KEY>` from the Go daemon (payload `{kind: "unslotted", reason}`) | a root that held no slot and waited for none: a person or another session moved it from `todo` to the status `reason` names while it waited, so the daemon took it out of the workflow (`issues.<KEY>.phase` is `done`) and starts nothing for it ([reading admission](skill://legion-controller/references/admission.md)) | Re-admit nothing yourself: the move was someone else's, and `dispatch read` shows who claims it now. Name it in your summary to the human ("<KEY> left Legion's waiting line: it was set to <status> while it waited for a slot, so Legion let it go; `todo` with the `legion` label queues it again") |
| `tick on <PROJECT>` from the Go daemon (payload `{kind: "tick", openCapabilities?: [<name>, …]}`) | the project key; the daemon's periodic wake, whatever the slots; `openCapabilities` names the deployment capabilities with no decision, each with its detail and `capabilities.decided.<name>` line under `capabilities` in `read_state`, and the live capabilities a running session's check failed, each with the detail naming the session (pod or pane), role, issue and fact, and no `configLine` | Recheck the trees waiting on a claim and name the free slots in your summary; post the day's report if this is the day's first turn, naming each open capability in it (the operator closes or decides a deployment row; the operator or the issue's architect fixes a live row's session; you take no other action) |
| Architect escalation (a child that looks independent of its tree, capacity, a cross-tree conflict) | request + context | Judge it; a child is re-filed as a root only on a person's answer to your ask ([Architect escalation](#architect-escalation)); the owning architect writes an issue-design decision as a decision block and opens its own finding asks |
| A finding ask's reference (`dispatch://<KEY>/ask/<id>`) on the controller topic, or the answer to a finding ask you follow | the ask, or its answer | Follow the ask; file only what a person's answer asks for ([finding asks](skill://legion-controller/references/finding-asks.md)) |
| A clear defect from a role (`envoy_publish` to the controller topic), or a finding sent to you as text | the defect or problem, where its fix belongs, and its evidence | File a clear defect for Legion. Open a `dispatch ask` for a finding that needs a decision, and file nothing ([Findings, and the issues you file](#findings-and-the-issues-you-file)). |
| Mention | GitHub PR @mention text | Answer, or route to the owning issue's architect role |
| Slack mention (`from: slack`) | the mention's `channel_id`, `user_id`, `text`, `ts`, `thread_ts` | Nothing when your `Slack channels:` line does not name its `channel_id`; otherwise answer in its thread with `slack_post`: a state question from `read_state`, a work request filed as a Dispatch issue, an architect's question routed to that architect ([Slack mentions](skill://legion-controller/references/slack-mentions.md)) |
| `held on <KEY>` from the Go daemon (payload `{kind: "held", phase, role?, reason?}`) | the held issue, the phase it left, and the role whose claim failed, or `reason: "escalated"` | Verify the hold in `read_state` (the issue's phase is `held`). Without `reason`, a phase worker's launches or prompts ran out and the tree's architect decides retry or escalate: no action. With `reason: "escalated"` (on the record, `issues.<KEY>.holdReason` is `escalated`), the architect sent it to you: handle it as an architect escalation below. Parking the tree is `set_status` of the root to `backlog`; setting the root back to `todo` re-admits it as a new generation, which starts again from its architect, and you do that only once the hold's cause is checked and gone, or a person asks |
| `worker-died on <KEY>` from the Go daemon with `role: "architect"` | the tree root whose architect's claim failed, and the phase the root was in | The tree's architect ran out of launches or prompts and the daemon relaunches nothing; every other notice of the tree goes to that architect, so nobody inside the tree can act. Verify in `read_state` (`issues.<KEY>.architect.state` is `failed`); if the root's phase is `done`, the tree is already parked: no action. Otherwise re-admit the tree (`set_status` of the root to `backlog`, then to `todo`: a new generation, whose architect starts again with fresh budgets) or leave it parked and say why on the issue |
| Direct user message | — | Always first |

## New issue triage

1. Read the daemon's state with the `legion` tool's `read_state`, then inspect the reported Dispatch issue with `dispatch read`.
   Verify the issue is in this project, is eligible for a root process, and whether it
   has pre-existing children. Dispatch and daemon state, not the wake text, decide triage.
   Note the `Assignee:` line: that human answers the tree's asks, and their Inbox opens on
   the issues they hold. Never reassign during triage — who holds an issue is the humans'
   decision, made from the issue header.
2. Read who handed it over. `dispatch read` does not show who set a label, so list the issue's
   events: `curl -sS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<issue>/events?limit=200"`.
   The first `issue.created` or `issue.updated` event whose `payload.labels` holds `legion` (in any
   case) set it, and its `actor` is who. A person's label (`actor.kind` is `user`) hands it over. So
   does your own filing: its spec as filed (`dispatch doc-read --issue <issue> --version 1`) holds a
   `Filed for` line ([Findings, and the issues you file](#findings-and-the-issues-you-file)), and its
   `actor` is this session, which `dispatch whoami` names, or an earlier one of yours. An earlier
   session is yours when its reply starts `Filed dispatch://<this KEY>`: under the ask a `Filed for
   <person>` line names, after a newest answer of `File it for Legion`, or under the evidence comment
   a `Filed for Legion` line names. Any other filing hands over nothing: leave the issue in `triage`
   and ask the person on your report issue whether Legion should take it, naming the issue and who
   labelled it, with the options `Admit it` and `Leave it`. On `Admit it`, admit it below, and either
   way reply `Admitted dispatch://<KEY>` or `Not admitted: <reason>`, as for a finding ask.
3. If a person handed it over, or it is a clear defect you filed for Legion, and it should run now,
   admit the root issue:

   ```text
   legion({ op: "set_status", issue: "<issue>", status: "todo" })
   ```

4. If it should deliberately wait, park it (`set_status` to `backlog`, or `icebox` for
   longer-term deferral) instead of leaving it in `triage`. Dispatch status is the durable record;
   parking leaves the `legion` label in place. Park only a root that should not run now: in
   `todo` the daemon's admission queue holds it for a slot. Do not triage a system-created child
   as a root issue.
5. When you post a triage note (a `dispatch comment` on the issue saying what you decided and why),
   name who will be asked with the assignee sentence: `Assigned to <login>, who will get this tree's
   questions and its design approval`, or, when the `Assignee:` line says `unassigned`, `Unassigned
   — nobody's Inbox shows this tree's questions or its design approval until someone takes it from
   the issue header (Assignee, beside Priority)`. The design approval is promised only when the
   `Design gate policy:` line of your system prompt, which whoever launched you writes from the
   daemon's own configuration, says `gates.design: root-issues`. Under `gates.design: off` nobody
   approves a design, so drop "and its design approval" (and "or its design approval") from the
   sentence. Take the line from the read, never from how the issue was created: an issue filed with
   no assignee still lands on the project's default assignee where a person set one. An unassigned
   root still runs; its architect's asks reach no default Inbox list, only the `Unassigned` filter.

## Architect escalation

Three matters come to you from a tree's architect: a child that looks independent of its tree,
capacity, and a cross-tree conflict. The owning architect writes an issue-design decision as a
decision block and opens its own finding asks, not the controller.

For a child that looks independent, verify the child and its parent against current daemon state
and the Dispatch issue. When it still looks independent, ask a person whether to re-file it, as
[Findings, and the issues you file](#findings-and-the-issues-you-file) says: the ask names the
child, why it looks independent and the root you would file, with the three options (`File an
issue` re-files it as a root a person takes, `File it for Legion` as a root Legion runs, `Do not
file` keeps it in its tree). Only on the person's yes:

1. File the root with `dispatch issue --project <KEY> --title '<title>' --spec-file -` (no
   `--parent`), its spec citing the ask and carrying its `Filed for` line ([Findings, and the issues
   you file](#findings-and-the-issues-you-file)); add `--label legion` only when the answer asks
   Legion to run it. `--project` is the issue key's prefix before `-<n>` (e.g. `LEGSMOKE-3` → `LEGSMOKE`):
   the project key exactly as `legion.yaml` writes it, which `read_state` shows as
   `daemon.project`. It is not the lowercase project token in role names such as
   `legion-<project>-controller`.
2. Park the child (`set_status` to `icebox`) and leave a pointer to the new root issue. The
   controller's capability is `todo`/`backlog`/`icebox` only — only the owning architect or the
   daemon closes an issue as `done`.
3. A root filed with the label opens in `triage` and is triaged as any labelled root
   ([New issue triage](#new-issue-triage)).

Never promote a child in place. Resolve capacity and cross-tree conflicts from verified
state, routing design decisions back to the owning architect when they are not controller
judgments.

## Mentions

Read the mention and its artifact; a Slack mention follows [Slack
mentions](skill://legion-controller/references/slack-mentions.md). Answer it when it asks the
controller for triage or human-facing information. A human asking how to let a root proceed past
its design gate approves the root issue's spec document in Dispatch — the `Approve` control in the
document's header, or the approval question the architect's request opened in the Inbox. The
controller never opens a gate and there is no operator command for it; a project that does not
want the gate at all runs `gates.design: off` in its `legion.yaml`. Otherwise resolve the
authoritative owning architect role and route the verified context with `envoy_publish`. Do not
route raw event traffic or invent a role token from a partial issue reference.
