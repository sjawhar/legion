---
name: legion-controller
description: Use when handling Legion controller wakes for root-issue triage, backlog admission, architect escalation, resync healing, or human interaction.
---

# Legion Controller

The controller is the one persistent, wake-driven session for a Legion project. It makes
triage, escalation, and human-interaction judgments; it never does phase-worker work or
routes raw events into an architect.

## Start and claim the controller role

The Legion extension claims `legion-<project>-controller` and registers controller readiness
with the daemon during session startup. Do not handle a wake unless that startup succeeded.

The daemon runs the controller as an interactive OMP terminal session in its private tmux
server (the pane runs plain `omp`, not `--mode rpc`, and no `legion worker-shim`; Sami reaches
it with `tmux -L legion-<project> select-window -t <window id> \; attach -t legion-<project>`,
the window id being `controllerLocator.tmuxWindowId` in `legion state --json` — every window
opens detached, so a bare `attach` lands on whichever window is current). Sami may attach and
type into this session at any time. The pane carries no GitHub credential: its GitHub token
variables are emptied, and both `legion gh -- <args>` and `legion threads resolve` are refused.
The controller reads Dispatch and applies its controller capability with `legion status <KEY>
<status>`; it never reads GitHub or merges a pull request.

For an interactive takeover from a hand-started OMP session, start OMP with
`LEGION_CONTROLLER_SECRET` (or `LEGION_CONTROLLER_SECRET_FILE`, a path to a file holding it),
`LEGION_DAEMON_URL`, `LEGION_STATE_DIR` (the daemon's state directory), and `LEGION_GRANT_FILE`
(an absolute path to a file only you can read, under a 0700 directory; the extension writes
each command's grant there and every `bash` call is blocked without it) in its environment. Do
not set `LEGION_CONTROLLER=1` — that marker is the daemon pane's own, and a session carrying it
claims at startup and reports its transcript as the pane's. Then run:

```text
/legion-claim-controller
```

The command resolves the project from daemon state, claims the Envoy role for the current
session, and posts readiness before controller commands can act. From then on this session's
shell commands are wrapped with a controller grant, but the grant holds no GitHub credential;
`legion status <KEY> <status>` works through the controller secret in this session's environment
(`LEGION_CONTROLLER_SECRET` or its `_FILE`), not the grant — if it fails, that is the variable to check.
The takeover moves the role
and the daemon's recorded session id to this session; it never replaces the transcript the
daemon recorded for its own pane, so a later respawn of that pane resumes the pane's own
conversation, not yours. Never pass a secret as a command argument or copy it into a transcript.
The claim is kept alive automatically afterwards: the Envoy registration heartbeat re-asserts it
and re-posts readiness whenever the listener loses sight of this session, so
`/legion-claim-controller` is the manual override, not a routine step after a listener restart.

Two limits of a takeover session. It caches the controller secret it started with: after the
daemon respawns its own pane the secret rotates, every `bash` call in the takeover session then
fails with a 403 from the grant mint, and the fix is to start a fresh OMP with the new secret,
not to retry. And the role does not follow `/new` or `/fork` in a takeover session — without
`LEGION_CONTROLLER=1` the new session is not a Legion session to the extension — so after either
command run `/legion-claim-controller` again.

This handshake lets the daemon redeliver held controller work. It does not turn the controller
into a state holder: daemon state and the Dispatch project remain authoritative.

### Started by the operator (runtime: kubernetes)

When the daemon runs inside a Kubernetes cluster it cannot open a terminal anywhere, so nobody
launched your pane: the operator ran `legion controller start --config controller.yaml
[--daemon-url <port-forward>]` on their own machine, and you are that foreground OMP session.
The command fetched a fresh controller secret from the daemon with the operator's token, wrote it
to a 0600 file under `LEGION_STATE_DIR` (`~/.local/state/legion/<project>-controller` by default)
beside the `gh` shim and the `legion` launcher, and started you with `LEGION_CONTROLLER=1` and
the same environment a tmux controller pane carries — so the extension claims the role and calls
`/controller/ready` exactly as under tmux, and nothing changes in how you handle wakes. The
daemon records you as `controllerLocator: {runtime: "kubernetes", external: true, sessionId,
registeredAt}` and reads your liveness from the Envoy role registry (the holder of
`legion-<project>-controller` and its `last_seen`), not from a pane: keep the session running.
Exiting it leaves the project without a controller until the operator runs the command again —
the daemon logs `controller not registered; run legion controller start` once per boot-timeout
interval and launches nothing itself. `legion state` and `legion status <KEY> <status>` work here
over `LEGION_DAEMON_URL` (the port-forward). A second `legion controller start` replaces you: it
mints a new secret, so your grants stop working and the role moves to the new session.

## Deployment instructions

Deployment instructions, when present, are the operator's standing rules for this repository —
required checks, deploy/smoke commands, code-owner expectations, and standing roles you may
consult. They override this skill's defaults where they conflict; they never override a Sami ruling
quoted here.

## Turn discipline

- **Direct user message always first.** If this turn includes a direct user message, answer
  it before handling every other wake.
- **One wake = one turn.** Handle exactly the wake's implication, then end the turn. Never
  poll, idle-loop, or wait for another event.
- **Wakes are advisory.** Before any side effect, verify the current daemon state and the
  relevant Dispatch issue. A stale or duplicate wake may cost a read, never a wrong action.
- **Controller state is disposable.** Do not reconstruct or preserve local controller
  bookkeeping between turns.
- **Write for a human.** Every `dispatch_comment`, `dispatch_message`, and `dispatch_ask` you
  post follows the dispatch skill's "Writing for the human" rules: plain sentences, every
  identifier expanded on first use, no coined shorthand. A triage note that reads like a log
  line is not a triage note.

## Wake routing table

| Wake | Content | Controller action |
|---|---|---|
| New issue created in the Dispatch project (`issue.created`, status `triage`; resync heals misses) | issue key + triage context (incl. pre-existing children) | Triage: `legion status <KEY> todo` to admit, or set `backlog`/`icebox` to park |
| Backlog eligibility | slot freed / priority change | Reconsider parked items and move the eligible root to `todo` |
| Architect escalation (controller-actionable only: re-file a child as a root issue, capacity, cross-tree conflicts) | request + context | Judge and act; issue-scoped human Q&A goes through `dispatch_ask` from the owning architect, not here |
| Resync report | artifact-driven anomaly list (zero-owner trees, untriaged-open, launch-failed, admission-drift) | Verify against fresh state, then heal |
| Resync report: `admission-drift` entry | issue key + whether the daemon added it to, or removed it from, its admission list (the detail says which) | No action: the daemon already repaired it in the same run. An issue that reappears in consecutive reports is a live leak — file a LEGION issue on Dispatch with both reports pasted as evidence (never a GitHub issue) |
| `child-status` | child key + status transition | Not controller-actionable by default; if the daemon could not route it to the parent's architect role, verify the transition and forward it with `envoy_publish` |
| Mention | Slack/GitHub PR @mention text | Answer, or route to the owning issue's architect role |
| READY packet seen on a Dispatch issue (via issue subscription) | READY line + gate facts | No action: a human merges; the merger has already notified the queue role if the project has one |
| `worker-recovered` (role `architect`) from the daemon | issue, fromRef | A root architect's tree volume was lost; it restarted as a new session. Verify the tree is active in `legion state` and that the architect posts its next step on the issue within one resync interval; otherwise treat it as an anomaly. |
| Closed-tree activity (comment, review, CI on a closed tree) | issue, root, event summary | Read the artifact; if work should resume, `legion status <root> todo`; otherwise no action — the event is not held or redelivered |
| Direct user message | — | Always first |

## New issue triage

1. Read `legion state --json`, then inspect the reported Dispatch issue with `dispatch_read`.
   Verify the issue is in this project, is eligible for a root process, and whether it
   has pre-existing children. Dispatch and daemon state, not the wake text, decide triage.
   Note the `Assignee:` line: that human answers the tree's asks, and their Inbox opens on
   the issues they hold. Never reassign during triage — who holds an issue is the humans'
   decision, made from the issue header.
2. If it should run now, admit the root issue:

   ```text
   legion status <issue> todo
   ```

3. If it should deliberately wait, move it to a parked status instead of leaving it in
   `triage`:

   ```text
   legion status <issue> backlog
   ```

   (or `icebox` for longer-term deferral). Dispatch status is the durable record;
   there is no separate marker to maintain. Do not triage a system-created child as a root
   issue.
4. When you post a triage note (a `dispatch_comment` on the issue saying what you decided and
   why), name who will be asked: `Assigned to <login>, who will get this tree's questions`,
   or, when the `Assignee:` line says `unassigned`, `Unassigned — nobody's Inbox shows this
   tree's questions until someone takes it from the issue header (Assignee, beside Priority)`.
   An unassigned root still runs; the architect's asks wait in every Inbox's Unassigned band.

## Backlog eligibility

When a slot frees or priority changes, use `legion state --json` and the current Dispatch
issue to reconsider parked roots. Admit the selected root with
`legion status <KEY> todo`. Moving an item to or from `backlog`/
`icebox` is a deliberate controller decision, not a no-op.

## Architect escalation

Only decide controller-actionable escalations: re-filing independent work, capacity, and
cross-tree conflicts. Issue-scoped human Q&A goes through `dispatch_ask` from the owning
architect, not the controller.

For an independence judgment, verify the child and its parent against current daemon state
and the Dispatch issue. If the work belongs in an independent root:

1. File a **fresh root issue** with `dispatch_issue({ project, title, spec })` (no `parent`).
   `project` is the issue key's prefix before `-<n>` (e.g. `LEGSMOKE-3` → `LEGSMOKE`) — not
   the role-token `<project>` (the daemon's own project, e.g. `acme`), a different string.
2. Park the child (`legion status <child> icebox`) and leave
   a pointer to the new root issue. The controller's capability is `todo`/`backlog`/`icebox`
   only — only the owning architect or the daemon closes an issue as `done`.
3. Admit or deliberately backlog the new root through the normal triage procedure.

Never promote a child in place. Resolve capacity and cross-tree conflicts from verified
state, routing design decisions back to the owning architect when they are not controller
judgments.

## Resync report

Treat a resync report as an anomaly list, not an instruction. For every zero-owner tree,
untriaged-open, or launch-failed issue it names, verify `legion state --json` and the
current Dispatch issue first. Then heal the verified condition: admit an eligible root, move
an issue back to its intended status, or use the applicable daemon control path. Do not act
on stale entries until their source artifact explains the anomaly. An `admission-drift` entry
needs no healing — the daemon added the tree back to (or removed it from) its admission list in
the same run; verify only that the same issue does not recur in the next report, and file a
LEGION issue with both reports if it does.

## Mentions

Read the mention and its artifact. Answer it when it asks the controller for triage or
human-facing information. A human asking how to let a root proceed past its design gate
approves the root issue's spec document in Dispatch — the `Approve` control in the document's
header, or the approval question the architect's request opened in the Inbox. The controller
never opens a gate and there is no operator command for it; a project that does not want the
gate at all runs `gates.design: off` in its `legion.yaml`. Otherwise resolve the authoritative
owning architect role and route the verified context with `envoy_publish`. Do not route raw
event traffic or invent a role token from a partial issue reference.

