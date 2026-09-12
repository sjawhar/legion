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

For an interactive takeover from a hand-started OMP session, start OMP with
`LEGION_CONTROLLER_SECRET` (or `LEGION_CONTROLLER_SECRET_FILE`, a path to a file holding it),
`LEGION_DAEMON_URL`, and `LEGION_STATE_DIR` (the daemon's state directory: the gh shim your
shell commands run through installs under `<state_dir>/worker-bin`, and every `bash` call is
blocked without it) in its environment. Do not set `LEGION_CONTROLLER=1` — that marker is the
daemon pane's own, and a session carrying it claims at startup and reports its transcript as the
pane's. Then run:

```text
/legion-claim-controller
```

The command resolves the project from daemon state, claims the Envoy role for the current
session, and posts readiness before controller commands can act. From then on this session's
shell commands are wrapped with a controller grant exactly like the daemon pane's, so
`legion gh -- <args>` and `legion status <KEY> <status>` work here. The takeover moves the role
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
| Resync report | artifact-driven anomaly list (zero-owner trees, untriaged-open, launch-failed) | Verify against fresh state, then heal |
| `child-status` | child key + status transition | Not controller-actionable by default; if the daemon could not route it to the parent's architect role, verify the transition and forward it with `envoy_publish` |
| Mention | Slack/GitHub PR @mention text | Answer, or route to the owning issue's architect role |
| READY from a merger (`notifications.role.<controller token>`) | `READY #<n> at <current sha> (approved at <approved sha>) for <KEY> (<pr url>)` + gate facts | Run the Merge queue gates against live GitHub; merge, or report the failed gate to the tree's architect |
| `pr.<n>.checks` settled on a PR with a pending READY | check rollup for the head | Re-run the Merge queue gates for that READY; merge, report, or keep waiting only if still pending |
| Closed-tree activity (comment, review, CI on a closed tree) | issue, root, event summary | Read the artifact; if work should resume, `legion status <root> todo`; otherwise no action — the event is not held or redelivered |
| Direct user message | — | Always first |

## New issue triage

1. Read `legion state --json`, then inspect the reported Dispatch issue with `dispatch_read`.
   Verify the issue is in this project, is eligible for a root process, and whether it
   has pre-existing children. Dispatch and daemon state, not the wake text, decide triage.
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

**READY message shape.** Defined once in `packages/pi-envoy/roles/merger.md` and mirrored here
verbatim. The first line is
`READY #<n> at <current sha> (approved at <approved sha>) for <KEY> (<pr url>)`: the pull
request number, the sha of the pull request's current head, the sha the reviewer's head-pinned
approval names, the issue key, and the pull request URL. The rest of the message is the PR
body's gate facts (the `## Verification` block). You need every field: the URL addresses the
pull request from this pane's working directory (which is not a checkout), the key finds the
tree's architect (below), the current sha is the only head you may merge, and the approved sha
anchors the two path-only compares in gates 5 and 6. The controller never verifies the
approval itself: whether a review must exist before merge is the repository's own
branch-protection or CODEOWNERS rule, which GitHub enforces at `pr merge` time and Legion
neither reads nor writes.

**Three shas.** This repository's flow leaves three commits that matter, and they are normally
all different. The *verified* sha is the head the tester and the reviewer worked at: the
`## Verification` block's own `CI`, `Thermo`, and `E2E` lines name it, and they must agree. After
that head is found clean the implementer pushes the `.legion/` handoff deletion and the reviewer
approves *that* head by name — the *approved* sha, one commit later. Retro then commits its
`docs/solutions/` learning on top — the *current* sha. READY carries the current and approved
shas; the verified sha you read from the block. The gates check the block at the verified sha
and prove, with two compares, that nothing but the `.legion/` deletion lies between verified and
approved, and nothing but `docs/solutions/` between approved and current.

**Gates.** Read them from live GitHub, never from the message or the PR body alone. Every `gh`
command takes the pull request URL, or `--repo <owner>/<repo>` taken from it, because this
session's working directory has no git remote to resolve a bare number against:

```text
legion gh -- pr view <pr url> --json headRefOid,mergeable,body
legion gh -- pr checks <pr url> --required --json name,state,bucket,link
legion gh -- api repos/<owner>/<repo>/rules/branches/<base branch> --jq '[.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context]'
legion gh -- api graphql -f query='query($owner:String!,$repo:String!,$n:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$n){reviewThreads(first:100){nodes{isResolved}}}}}' -F owner=<owner> -F repo=<repo> -F n=<n>
legion gh -- api repos/<owner>/<repo>/compare/<verified sha>...<approved sha> --jq '{status, files: [.files[].filename]}'
legion gh -- api repos/<owner>/<repo>/compare/<approved sha>...<current sha> --jq '{status, files: [.files[].filename]}'
```

1. **head**: `headRefOid` equals the `<current sha>` in the READY. Any other head is a different
   pull request as far as this READY is concerned.
2. **checks**: `pr checks --required` succeeds (exit 0) with a non-empty list in which every
   row's `bucket` is `pass`. That is the only green. A `pending` row is not green; a `fail` row
   never merges (see Flake). The command never returns an empty list: when nothing has reported
   at the head yet it exits 1 with `no checks reported on the '<branch>' branch`, and when checks
   exist but none is required it exits 1 with `no required checks reported on the '<branch>'
   branch` — a freshly pushed head has no check runs for a few seconds, and a head that conflicts
   with the base never gets any. Either exit is **pending**, never green: subscribe to
   `pr.<n>.checks` exactly as for a running check and re-run the gates on that wake. Two
   exceptions, both read from the repository, never from the absence of rows:
   - A repository that genuinely requires no checks: the `rules/branches/<base branch>` query
     above returning `[]` is what lets this gate hold with no check rows.
   - A private repository on GitHub's free plan cannot define required checks at all: that same
     query answers HTTP 403 with a body saying
     `Upgrade to GitHub Pro or make this repository public to enable this feature`. Match that
     sentence exactly — any other 403 is a permission error and stays an error, never "no
     required checks". Under it, gate 2 requires every check reported on the head to be green
     instead: `legion gh -- pr checks <pr url> --json name,state,bucket,link` (without
     `--required`) exits 0 with at least one row and every row's `bucket` is `pass`. A `pending`
     row is pending, a `fail` row never merges, and no rows (the exit-1 `no checks reported`)
     stays pending exactly as above. This is stricter than "no required checks, merge", and
     GitHub still enforces whatever protection the repository does have at `pr merge` time, so a
     wrong read costs a refused merge reported to the architect, never an unprotected one.
3. **threads**: zero unresolved review threads (the count of `isResolved: false` is 0).
4. **mergeable**: `mergeable` is not `CONFLICTING` and not `UNKNOWN`.
5. **cleanup only**: `compare/<verified sha>...<approved sha>` reports `status` `identical` or
   `ahead`, and every path in `files` starts with `.legion/` — the handoff deletion the reviewer
   directed, and nothing else. Anything else between the two is the failed gate
   `cleanup changed more than .legion`.
6. **retro only**: `compare/<approved sha>...<current sha>` reports `status` `identical` or
   `ahead`, and every path in `files` starts with `docs/solutions/`. Anything else between the
   two is the failed gate `head moved beyond retro`: the approval no longer covers the head.
7. **verification block**: the PR body's `## Verification` block (the template in
   `skills/legion-worker/SKILL.md`) is complete and current at the verified sha. The tester
   fills the `E2E` line before review; the reviewer writes the `Thermo` line at the head it
   audited; approval lands one commit later on the cleanup head; so the block names the verified
   sha, never the approved or the current one. Line by line: the `CI` line names a run and
   reports success at one sha; the `Thermo` line names the same sha and a verdict, unless the
   pull request is docs-only, in which case the template omits that line entirely; the `E2E`
   line names the same sha and has a `Negative control` line — those lines agreeing on one sha
   is what defines the verified sha; the `Threads` line reports `0 unresolved` (its per-thread
   lines name fixing commits, never the head — do not look for a sha there); the `Fast-follow`
   and `Chain` lines are filled in. No `<placeholder>` text remains anywhere in the block.

When all seven hold, merge:
`legion gh -- pr merge <pr url> --squash --match-head-commit <current sha>`. The head pin makes
GitHub refuse the merge if a push landed after gate 1 read the head; that refusal is a failed
`head` gate, reported like any other. The grant your `bash` call carries is the controller's
own, the only grant the daemon honours for a merge; the merge runs under the implement App's
identity and the repository's own rules (branch protection, CODEOWNERS). Whether a human must
approve first is that repository's setting — you neither read nor bypass it, and you never
admin-merge without an explicit deployment grant from Sami for that specific merge.

**Failed gate.** Reply to the tree's architect naming the gate (`head`, `checks`, `threads`,
`mergeable`, `cleanup changed more than .legion`, `head moved beyond retro`, or
`verification block`) and the evidence you read (the shas, the check name and run link, the
thread count, the `mergeable` value, the offending paths from the compare). Do not merge, do not
retry on a timer. The architect fixes through the phases.

**Flake.** A required check that failed for a reason unrelated to the change (a runner outage,
a rate limit, a known-flaky job) may be rerun once:
`legion gh -- run rerun <run-id> --failed --repo <owner>/<repo>`, the run id taken from the
failing row's `link` (`https://github.com/<owner>/<repo>/actions/runs/<run-id>/job/<job-id>`).
Then stop. The rerun's result reaches you as a `pr.<n>.checks` wake; re-run the gates then.
A second failure is a failed gate, reported as above.

**Conflicts and unknown mergeability.** `mergeable == CONFLICTING` is the only reason to ask
for a rebase: reply to the tree's architect asking for one. Never request a rebase for any other
reason — the CI queue is long and slow, and an unnecessary rebase clogs it for every other pull
request. `mergeable == UNKNOWN` means GitHub has not finished computing it: do not merge, do
not poll; re-read on the next `pr.<n>.checks` wake.

**Pending READY.** A READY that cannot merge yet only because checks are still running, none
has reported at the head yet (gate 2's `no checks reported` exit), a flake rerun was issued, or
`mergeable` is `UNKNOWN` is pending. Subscribe to that pull request's
events so its settlement wakes you:

```text
envoy_subscribe({ topics: ["notifications.github.<owner>.<repo>.pr.<n>", "notifications.github.<owner>.<repo>.pr.<n>.checks"] })
```

On that wake, re-run the gates against the shas from the READY in your conversation, then
`envoy_unsubscribe` those topics once you have merged or reported a failed gate. The controller
never polls; READY and `pr.<n>.checks` are the only wakes. If you were resumed and no longer
have the READY in your conversation, ask that issue's merger (its token is the `roles` key in
`legion state --json` whose `issue` is `<KEY>` and whose `role` is `merger`; publish to
`notifications.role.` followed by that key) to republish it; never guess a sha.

**Finding the tree's architect.** Never hand-format a role token: the daemon lower-cases the
issue key inside it (`LEGION-16` becomes `legion-16`) and rejects any other shape, so a token
you assemble from `<KEY>` never matches a live role. Read it instead: `legion state --json`
gives `issues[<KEY>].parent`; follow `parent` until it is absent — that key is the root (the
`trees` map lists the same roots). Then take the `roles` key whose `issue` equals that root and
whose `role` is `architect`, and publish to `notifications.role.` followed by that exact key.
Every registered root architect and phase worker appears in `roles`, so the lookup is
unambiguous. (Phase workers get an addressing line in their system prompt; the controller does
not, so state is your only source.)

**After a successful merge, publish nothing to the architect.** The daemon derives
`{type:"pr-merged", pr, mergeCommitSha}` from GitHub's own merged webhook and routes it to the
tree's architect itself. A second copy from you would make the architect run its sign-off twice.

**Policy questions go to Sami.** Whether a pull request should merge at all, whether an admin
merge is warranted, or a gate that looks wrong for this repository is not a controller judgment:
ask with `dispatch_ask` on the issue, in plain sentences, and leave the READY pending until the
answer arrives.
