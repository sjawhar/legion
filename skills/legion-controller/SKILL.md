---
name: legion-controller
description: Use when handling Legion controller wakes for root-issue triage, backlog admission, architect escalation, resync healing, merge-queue READY handling, or human interaction.
---

# Legion Controller

The controller is the one persistent, wake-driven session for a Legion project. It makes
triage, escalation, and human-interaction judgments; it never does phase-worker work or
routes raw events into an architect.

## Start and claim the controller role

The Legion extension claims `legion-<project>-controller` and registers controller readiness
with the daemon during session startup. Do not handle a wake unless that startup succeeded.

The daemon runs the controller as an interactive OMP terminal session in its private tmux
server (`tmux -L legion-<project> attach` reaches it; the pane runs plain `omp`, not
`--mode rpc`, and no `legion worker-shim`). Sami may attach and type into this session at any
time. `LEGION_STATE_DIR` is in its environment, so `legion gh -- <args>` works here through the
same gh shim phase workers use: every `bash` call is wrapped with a short-lived controller grant,
and that grant is the only one the daemon lets merge a pull request.

For an interactive takeover, start OMP with `LEGION_CONTROLLER_SECRET` (or
`LEGION_CONTROLLER_SECRET_FILE`, a path to a file holding it) and `LEGION_DAEMON_URL` in its
environment, then run:

```text
/legion-claim-controller
```

The command resolves the project from daemon state, claims the Envoy role for the current
session, and posts readiness before controller commands can act. It retains the environment
capability for `legion({ op: "set_status", issue, status })`. Never pass a secret as a command argument
or copy it into a transcript. The claim is kept alive automatically afterwards: the Envoy
registration heartbeat re-asserts it and re-posts readiness whenever the listener loses sight of
this session, so `/legion-claim-controller` is the manual override, not a routine step after a
listener restart.

This handshake lets the daemon redeliver held controller work. It does not turn the controller
into a state holder: daemon state and the Dispatch project remain authoritative.

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
| New issue created in the Dispatch project (`issue.created`, status `triage`; resync heals misses) | issue key + triage context (incl. pre-existing children) | Triage: `legion({ op: "set_status", issue, status: "todo" })` to admit, or set `backlog`/`icebox` to park |
| Backlog eligibility | slot freed / priority change | Reconsider parked items and move the eligible root to `todo` |
| Architect escalation (controller-actionable only: re-file a child as a root issue, capacity, cross-tree conflicts) | request + context | Judge and act; issue-scoped human Q&A goes through `dispatch_ask` from the owning architect, not here |
| Resync report | artifact-driven anomaly list (zero-owner trees, untriaged-open, launch-failed) | Verify against fresh state, then heal |
| `child-status` | child key + status transition | Not controller-actionable by default; if the daemon could not route it to the parent's architect role, verify the transition and forward it with `envoy_publish` |
| Mention | Slack/GitHub PR @mention text | Answer, or route to the owning issue's architect role |
| READY from a merger (`notifications.role.<controller token>`) | `READY #<n> at <sha> for <KEY> (<pr url>)` + gate facts | Run the Merge queue gates against live GitHub; merge, or report the failed gate to the tree's architect |
| `pr.<n>.checks` settled on a PR with a pending READY | check rollup for the head | Re-run the Merge queue gates for that READY; merge, report, or keep waiting only if still pending |
| Closed-tree activity (comment, review, CI on a closed tree) | issue, root, event summary | Read the artifact; if work should resume, `legion({ op: "set_status", issue: root, status: "todo" })`; otherwise no action — the event is not held or redelivered |
| Direct user message | — | Always first |

## New issue triage

1. Read `legion state --json`, then inspect the reported Dispatch issue with `dispatch_read`.
   Verify the issue is in this project, is eligible for a root process, and whether it
   has pre-existing children. Dispatch and daemon state, not the wake text, decide triage.
2. If it should run now, admit the root issue:

   ```text
   legion({ op: "set_status", issue: "<issue>", status: "todo" })
   ```

3. If it should deliberately wait, move it to a parked status instead of leaving it in
   `triage`:

   ```text
   legion({ op: "set_status", issue: "<issue>", status: "backlog" })
   ```

   (or `status: "icebox"` for longer-term deferral). Dispatch status is the durable record;
   there is no separate marker to maintain. Do not triage a system-created child as a root
   issue.

## Backlog eligibility

When a slot frees or priority changes, use `legion state --json` and the current Dispatch
issue to reconsider parked roots. Admit the selected root with
`legion({ op: "set_status", issue, status: "todo" })`. Moving an item to or from `backlog`/
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
2. Park the child (`legion({ op: "set_status", issue: child, status: "icebox" })`) and leave
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
on stale entries until their source artifact explains the anomaly.

## Mentions

Read the mention and its artifact. Answer it when it asks the controller for triage or
human-facing information. Otherwise resolve the authoritative owning architect role and
route the verified context with `envoy_publish`. Do not route raw event traffic or invent a
role token from a partial issue reference.

## Merge queue

The controller is the project's merge queue. A merger reports a pull request ready by
publishing to the controller topic; the controller re-reads every gate from live GitHub and
merges, or tells the tree's architect exactly which gate failed. The merger's report is a
claim, never evidence.

**READY message shape.** The first line is `READY #<n> at <sha> for <KEY> (<pr url>)`: the pull
request number, the head sha the reviewer approved, the issue key, and the pull request URL. The
rest of the message is the PR body's gate facts (CI run, resolved threads, thermo verdict, E2E,
chain). The issue key is how you find the tree's architect (below); the sha is the only head you
may merge.

**Gates.** Read them from live GitHub, never from the message or the PR body alone:

```text
legion gh -- pr view <n> --json headRefOid,mergeable,reviewDecision,statusCheckRollup,body
legion gh -- api graphql -f query='query($owner:String!,$repo:String!,$n:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$n){reviewThreads(first:100){nodes{isResolved}}}}}' -F owner=<owner> -F repo=<repo> -F n=<n>
```

1. `headRefOid` equals the `<sha>` in the READY. Any other head is a different pull request as
   far as this READY is concerned.
2. Every required check in `statusCheckRollup` is green. A check that is still running or
   queued is pending, not green. A known red check never merges.
3. Zero unresolved review threads (`isResolved: false` count is 0).
4. `mergeable` is not `CONFLICTING` and not `UNKNOWN`.
5. The PR body's `## Verification` block is complete at that head: its `CI`, `Threads`, and
   `E2E` lines name this `<sha>`.

When all five hold, merge: `legion gh -- pr merge <n> --squash`. The grant your `bash` call
carries is the controller's own, the only grant the daemon honours for a merge; the merge runs
under the implement App's identity and the repository's own rules (branch protection,
CODEOWNERS). Whether a human must approve first is that repository's setting — you neither
read nor bypass it, and you never admin-merge without an explicit deployment grant from Sami for
that specific merge.

**Failed gate.** Reply to the tree's architect naming the gate (`head`, `checks`, `threads`,
`mergeable`, or `verification block`) and the evidence you read (the sha, the check name and
run id, the thread count, the `mergeable` value). Do not merge, do not retry on a timer. The
architect fixes through the phases.

**Flake.** A required check that failed for a reason unrelated to the change (a runner outage,
a rate limit, a known-flaky job) may be rerun once: `legion gh -- run rerun <run-id> --failed`.
Then stop. The rerun's result reaches you as a `pr.<n>.checks` wake; re-run the gates then.
A second failure is a failed gate, reported as above.

**Conflicts and unknown mergeability.** `mergeable == CONFLICTING` is the only reason to ask
for a rebase: reply to the tree's architect asking for one. Never request a rebase for any other
reason — the CI queue is long and slow, and an unnecessary rebase clogs it for every other pull
request. `mergeable == UNKNOWN` means GitHub has not finished computing it: do not merge, do
not poll; re-read on the next `pr.<n>.checks` wake.

**Pending READY.** A READY that cannot merge yet only because checks are still running, a flake
rerun was issued, or `mergeable` is `UNKNOWN` is pending. Subscribe to that pull request's
events so its settlement wakes you:

```text
envoy_subscribe({ topics: ["notifications.github.<owner>.<repo>.pr.<n>", "notifications.github.<owner>.<repo>.pr.<n>.checks"] })
```

On that wake, re-run the gates against the `<sha>` from the READY in your conversation, then
`envoy_unsubscribe` those topics once you have merged or reported a failed gate. The controller
never polls; READY and `pr.<n>.checks` are the only wakes. If you were resumed and no longer
have the READY in your conversation, ask the merger's role topic for that issue
(`notifications.role.legion-<project>-<KEY>-merger`) to republish it; never guess a sha.

**Finding the tree's architect.** The READY names the issue key. `legion state --json` gives
`issues[<KEY>].parent`; follow `parent` until it is absent — that key is the root — and reply
to `notifications.role.legion-<project>-<root>-architect` (the `trees` map lists the same
roots). Never hand-format a token from a partial reference.

**After a successful merge, publish nothing to the architect.** The daemon derives
`{type:"pr-merged", pr, mergeCommitSha}` from GitHub's own merged webhook and routes it to the
tree's architect itself. A second copy from you would make the architect run its sign-off twice.

**Policy questions go to Sami.** Whether a pull request should merge at all, whether an admin
merge is warranted, or a gate that looks wrong for this repository is not a controller judgment:
ask with `dispatch_ask` on the issue, in plain sentences, and leave the READY pending until the
answer arrives.
