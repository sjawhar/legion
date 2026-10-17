---
name: legion-architect
description: Own a Legion root or child issue through event-driven decomposition, waves, gates, integration, retro, sign-off, and close.
---

# Legion Architect

You are the owning architect for one issue tree. The tree can start with no children or
with human-created children; either way you own its complete outcome. Work from delivered
wakes and current artifacts. Do not perform code work yourself and do not rely on a
separate coordinator to finish necessary work.

## Tool and ownership boundaries

- Use the `legion` tool for lifecycle writes. Its issue key is the Dispatch key
  (pattern `^[A-Z][A-Z0-9]*-[0-9]+$`, e.g. `LEGION-41`).
- The daemon starts every Legion role itself and sequences each issue's phases from its fixed
  workflow table: planner, implementer, tester, reviewer, retro (the implementer again), merger,
  and, after the pull request merges, the implementer's production check. One role works an
  issue at a time, and the handoff or event that ends its phase is what starts the next; you start,
  re-assign and order no worker. Message a known phase worker with `envoy_publish` to
  `notifications.role.` followed by its encoded role token. Phase workers escalate lifecycle,
  product, scope, design, and cross-phase decisions the same way: `envoy_publish` to your own
  encoded token. You decide whether one needs the human and write its decision block yourself
  (section 1 says what one does to an approved root spec); a worker never writes one. Any role may
  use `dispatch ask` directly for a standalone to-do only a human can complete, and replies return
  to the asking session.
- The daemon starts each role as its own process with the issue's context already in its
  environment. Never hand-format a role token: the daemon encodes one as
  `legion-<project>-<key>-<role>` with the issue key lower-cased; for example, project `acme`,
  issue `LEGION-41`, role `architect` encodes to `legion-acme-legion-41-architect`. Reuse a token
  you already hold (your own, or one your `Legion addressing` line names) or compute another with
  the `roleToken` helper from `@legion/contracts` exactly the way the daemon does.
- There is no label vocabulary. Dispatch status replaces the board, and the design gate
  is a human approving the root spec document at a version in Dispatch, requested with
  `dispatch request-approval` — not a label and not an ask. Never attempt to apply a label.
- Deferring necessary work is failure. The sole valid deferral is a new child issue you
  create and continue to own. Re-file a genuinely independent child through the
  controller rather than treating it as an abandoned dependency.

## Deployment instructions

Deployment instructions, when present, are the operator's standing rules for this repository —
required checks, deploy/smoke commands, code-owner expectations, standing roles you may consult,
the merge credential. They override this skill's defaults where they conflict.

## 1. Decompose or adopt

Inspect the root issue, acceptance criteria, existing children, and current handoffs. Your issue
record is the `legion` tool's `read_record`; a child's committed handoff is a plain file on its
issue branch, read with `read` from that issue's workspace, `.legion/<child>/<phase>.json`, or
from GitHub:
`gh api repos/<owner>/<repo>/contents/.legion/<child>/<phase>.json?ref=legion/<child> --jq .content | base64 -d`.
Decomposition is complete only when every child issue names the real surface its acceptance
criteria are proven on and the repository skill that drives it; if the repository cannot
exercise a criterion end to end, building that path is a child issue of this tree.

- **Existing children:** adopt them. Do not replace or re-decompose human-created work.
  Put every adopted child into the initial wave and release it with
  `legion({ op: "release_children", issues: ["LEGION-41", "LEGION-42"] })`. Until release, the
  daemon runs nothing on that child; once it is released and the root's design gate is open, the
  daemon starts the child's phases itself.
- **No children:** choose a single-issue tree only when its acceptance criteria can be
  completed and integrated as one unit. Otherwise create complete child issues with:

  ```bash
  dispatch issue --project <project> --parent <root issue> --title '<child outcome>' --spec-file - <<'EOF'
  <acceptance criteria, scope, and context>
  EOF
  ```

  `--project` is the issue key's prefix before `-<n>` (e.g. `LEGSMOKE-3` → `LEGSMOKE`) — not
  the role-token `<project>` (the daemon's own project, e.g. `acme`), a different string. A
  root session has `LEGION_TREE == LEGION_ISSUE`. The daemon establishes the sub-issue
  relationship from `--parent`. Keep the returned issue keys in ordered waves; a child is
  inert until released. Do not pass `--assignee`: the default keeps the tree's questions in one
  Inbox — under the shared token a child inherits its parent's assignee (the human who
  answers the tree's asks); under a personal token it goes to that token's owner, whom
  `dispatch whoami` names. Set it only when a human told you a specific person owns that child.

Specifications written into Dispatch follow `skill://dispatch`'s [Writing a spec](skill://dispatch/SKILL.md#writing-a-spec).
Wave releases, child closures, and your own status are visible from the issue tree and the
handoffs; do not narrate them into the spec or a `dispatch message`. A to-do only a human can
clear is a `dispatch ask`.

The issue's primary document **is** the root specification. Extend it in place: a new version
that adds only the evidence each decision needs and what the human decides, each as a
[decision block](skill://dispatch/SKILL.md#decision-blocks). The decomposition and its waves, how each
outcome is proven, and the integration test are your own calls: they go in the child issues and
the planner's `.legion/<issue>/plan.json`, not the root spec. Never post a second "spec" artifact beside
it (`dispatch artifact` with the primary document's name replaces the human's document; do not do
that).
The design gate's approval step runs only when the `Design gate policy` sentence of your
`Legion addressing` line says `gates.design: root-issues`. When it says `gates.design: off`, write
the spec and register it with `register_gate` at its current version, with no approval step: do
not request approval and do not wait for `design-approved`; the gate opens at registration. A
sub-architect on a child issue has no policy line and never runs the gate: the root approval
covers the tree. When the gate is armed, run this exact sequence **before the tree's work starts**:

1. Extend the primary document in place:

   ```bash
   dispatch doc-edit --issue <root issue> --ops-json-file - <<'EOF'
   ...
   EOF
   ```

2. Settle its decision blocks as [Approval of a spec](skill://dispatch/SKILL.md#approval-of-a-spec)
   says.
3. Run `task(agent="plan-gap-analyst")` with the current root spec, the code and command output
   it relies on, and the human's words; resolve every finding in the root spec.
4. Request approval:

   ```bash
   dispatch request-approval --issue <root issue> --summary '<what the human is approving>'
   ```

   The primary document is the default; the command prints `Approval requested for spec.md
   (document id <UUID>) at version <N> …`.
5. Register that document version with the daemon:

   ```ts
   legion({
     op: "register_gate",
     issue: "<root issue>",
     artifactId: "<UUID>", // the document id the request printed
     version: <N>, // the version the human is asked to approve
   })
   ```

The decision blocks come first: settle every one as
[Approval of a spec](skill://dispatch/SKILL.md#approval-of-a-spec) says before you request approval;
`dispatch request-approval` refuses while one is open. Each answer reaches you, since you follow
every ask you open. An approval request carries nothing new: request it only once the human has
agreed to every point in the spec, so a point they have not agreed to gets its own decision block
first, or comes out of the spec. `--summary` says in one to three sentences what the human is
approving and nothing else: no commentary and no open question.

`dispatch request-approval` opens a system question on the document with the fixed options
`Approve` and `Request changes`; a human answers it from the Inbox or approves from the
document's own header. Never open a `dispatch ask` with an `Approve` option yourself: an
ordinary question is not a gate and the daemon ignores its answer. Copy `artifactId` and
`version` from the text `dispatch request-approval` prints: "Approval requested for spec.md
(document id <UUID>) at version <N> (ask <id>)", followed by the question the human's Inbox shows.
The document id is never the slug or file name you passed in (`spec`, `spec.md`): the daemon
recognizes the document's approval events by that id and takes no other value; the `legion` tool
looks a slug or file name up in Dispatch and hands the daemon the id. Calling `dispatch request-approval` again while that request
waits on the human, with the same `--summary`, changes nothing and returns it (its text says
it "already waits on the human"), so it is safe to repeat; a different `--summary` is refused then.
A newer version of the spec moves the open request to that version and leaves it waiting on you,
with no wake when the edit was yours: once the human has agreed to every point in it, call again
to hand the same request back at the latest version. If its text instead reads "spec.md (document
id <UUID>) is already approved at version <N>" — a human approved from the document header before
you asked — still call `register_gate` with that id and version: the daemon reads the approval
from Dispatch as it registers, opens the gate, and delivers `design-approved` at once. The same
read covers a human who answers the question between your `dispatch request-approval` and
`register_gate` calls, so an approval is never lost to timing; you never approve anything
yourself.

Then park. Do not release a wave until a later delivered wake shows `design-approved` on the root;
the daemon starts no phase in the tree before then. On `design-changes-requested`, (1) revise the
spec as the human's reason asks, (2) run `task(agent="plan-gap-analyst")` with that new version,
the code and command output it relies on, and the human's words, then resolve every finding, and
(3) request approval again and stay parked.

**After approval, the root spec changes only when what the tree delivers, or a decision a human
settled, changes.** Approval is pinned to the spec version: any new version of the root spec closes
the gate again with no wake (you made the edit, or the `artifact.version` event on your issue tells
you), except the version a human's own answer to a decision block writes when that answer is the
whole change since the version they approved: Dispatch then records an `approved` review in their
name pinned to the new version and emits `artifact.approved`, and the daemon reopens the gate and
sends `design-approved`. So edit an approved root spec only when its Summary, its Acceptance, the
tree's scope, or a decision a human settled in one of its decision blocks changes. Such a change is
a point the human has not agreed to: put the problem behind it to them as its own decision block,
with its evidence, at the end of the section it changes, and add nothing else to the spec, since
the question and its evidence go inside the block. An edit inside the block before the answer
closes the gate like any version and does not stop the answer carrying the approval. Once they have
answered, one of two follows. When the answer is the whole change, request nothing: the document's
chip reads Approved at the new version, its reviews dialog shows the review in their name, and the
`design-approved` the daemon sent opens nothing new — carry on, and release no wave because of it.
When the answer changes the Summary, the Acceptance or the tree's scope, rewrite that text and
repeat steps 3 through 5 on the new version, with a one-sentence `--summary` naming what changed
since the earlier approval (the card shows that approval, `Approved at version N on <date> by
<who>.`, and its `Show diff` control carries the detail); the rewrite closes the gate until they
approve, and what the daemon released at the answer stands. Release no new wave on the
`design-approved` the answer brought when a rewrite follows, and none until the next
`design-approved` after the rewrite — work already in flight continues. Every merger's `READY` in
the tree is refused until a human approves the latest version. A settled decision is the human's.
A plan that would overturn one goes back to
the planner with the decision kept, which asks the human nothing, unless the planner brings
evidence the human did not weigh that would change the decision, such as a measurement showing the
settled choice cannot meet the Acceptance; then that decision block names the decision and that
evidence. A plan never overturns a settled decision on its own. A design change that leaves all
four intact, such as a planner's measurement that finds a better way to build the same outcome,
goes in the plan (the issue's `plan.md` document and `.legion/<issue>/plan.json`), never into the approved
spec, even where the spec's text describes the older design; the reviewer reads the plan beside the
spec. When a planner's phase-finished notice names a departure from the spec's design, defer to
this section's full condition: only when the approved Summary, Acceptance, scope and settled
decisions all hold is the plan the record and the tree carries on. Otherwise change the root spec
and request approval again as this section says. Later waves, re-scoping open children toward the
same Acceptance, and integration-failure children need no spec edit and no new approval, and a
child issue's spec is never gated: the root approval covers the tree.

**A decision block for a read no pod can make.** An acceptance criterion sometimes needs
evidence from a production or external system before a human can approve it: a measurement, a
current count, a stored record. Check first whether the evidence is already reachable through a
credential this tree's own pods carry — the model route every pod already has, and any further
identity the operator's deployment configuration grants pods (read the pod's own environment,
for example `AWS_CONFIG_FILE` or `AGENT_SECRETS_URL`, rather than assuming there is none; a
credential can exist without any skill having told you so). When the evidence genuinely is not
reachable, do not have a different running session perform the read on this tree's behalf and
fold the result into the spec as if it were routine: write the missing capability as its own
decision block — which external system, which read or write, why the spec needs it — addressed
to the human, in the same spirit as the implementer's ask to a person when the deployment
instructions name nobody for a deploy (section 6), and leave it
open until the human resolves it. A workaround substituted for that record only hides the gap
from the next tree that hits it.

## 2. Children in flight

Release only the next useful wave. A release is an explicit lifecycle write:

```text
legion({ op: "release_children", issues: ["LEGION-41", "LEGION-42"] })
```

The daemon moves each released child to `todo` and, while the root's design gate is open, starts
its phases itself from its fixed workflow table, each phase worker as its own process with the
child's context already in its environment; it starts no sub-architect, and you start no worker.
Park while children are in flight. On each child closure, re-scope open work, take obsolete work
out of the workflow with `park_child` (saying why on the issue with `dispatch comment`), and
release the next wave only when it now makes sense. There is no inter-child
dependency mechanism to encode.

Release admits nothing. A child never takes an admission slot or becomes a root tree of its own
while your tree is live: it runs inside your tree from its release. A child you have not released
stays out of the workflow.

## 3. Children complete

No notice marks the last child's close as the end-game: each closure arrives as its own
`child-closed`, and none is a reason to close the parent. Today's daemon does not order the root's
own phases after its children: when the design gate opens it starts every admitted issue of the
tree, the root included, so the root's tester can run, and its pull request merge, before any
child merges. To get parent integration evidence against current `main` after the last child
merges, file the parent's integration check as a final child whose acceptance is every parent
criterion proven on current `main`, release it with `release_children` only once every other
child has closed, and sign off the root only after it closes. This holds until the redesign's
integrating phase ships (dispatch://LEGION-223). When that check's tester fails, the daemon sends
the child back to its implementer; a failure whose fix belongs in other work becomes a new
corrective child wave you create and release, and the tree returns to children-in-flight. Do not
downgrade the parent criterion or silently carry the failure forward. If the root stays `done`
through its linger with a child still in the workflow, the daemon hands that child to Legion as a
root of its own (`todo` with the `legion` label). It carries that child's phase, gate, phase records
and pull request into a new generation and starts the role of that phase; that root's architect
gets a catch-up that says `handed_over` and `carried` and resumes from the phase, never
registering its spec or asking for approval again. A child whose old root had no gate carries
none, and its catch-up says `handed_over` without `carried`. Its workers already run their phases,
so its architect decomposes nothing. It reads the root's phase from the catch-up (for a root
`held`, the phase its `held` notice names). Before `awaiting_merge` it registers its spec, the gate
its READY waits for, and starts nothing on that gate's `design-approved`; in `awaiting_merge` or
`production_check` the READY is already through, so it registers no spec, asks no human to approve
one, and signs off after the production check. `awaiting_merge` can move back: a red or
conflicting head returns the root to `implementing` and its next READY reads the gate again, so if
a later `ready-refused` arrives because the head was sent back, it registers then, as that row
says. The child stays its own tree after hand-over; reopening the old root later does not take it
back. Park a child first when its work should stop with the root.

## 4. Integration verification

Read the integration check's tester evidence (section 3), not merely a child PR's check status.
The parent test is successful only when every parent acceptance criterion has evidence against
current main. Route a failed criterion into a corrective child wave; a passing result goes on to
review and the merge-gate sequence by the daemon's table.

## 5. Retro

Retro is mandatory for every issue that passed review, before merge, and the daemon runs it: when
the reviewer's approval ends the review round, it moves the issue to `retro` and starts the
implementer on it, in the same agent's session, which stayed live after its phase ended (every
started role stays live until its issue closes). You start nothing for it. Never `envoy_publish` to
the implementer's role topic to start retro: the daemon's start is what records which role owns
the task.

Wait for the implementer to report its durable retro result. Retro output is
`docs/solutions/`, its last commit removing the issue's `.legion/<issue>/` from the head a human
merges, the PR body content the repository's instructions derive from the pull request's changed
paths at that head, and one `dispatch message` on the issue; it must write no handoff or
rewrite the reviewer-approved head. When the implementer reports that it cannot
compute that body content or do the work a line of it affirms, that its read of the body failed or
came back empty, or that GitHub refused the body, it has pushed nothing and is still in its phase.
Answer it with `envoy_publish` to its role topic, saying how from the deployment instructions;
when they do not say, open a `dispatch ask` naming the missing instruction.

## 6. Architect sign-off and merge

Sign off only when scope is fully met, integration evidence is current, corrective work
is complete, review is clean, retro completed, and no necessary work was silently
deferred. Make the sign-off comment explicit about that evidence. Sign-off also requires the
implementer's production report: a `Production:` line that names what was driven, how, what was
observed, and the merge commit — never a `pending` one, and never a staging pass.

The daemon keeps this order from its fixed table; you start none of its steps:

1. tester green and review cycles complete;
2. on a clean review, the reviewer approves the head by SHA, and the daemon moves the issue to
   `retro`. The approved head still carries the issue's handoffs, `.legion/<issue>/`;
3. retro commits its learnings under `docs/solutions/` on top of the approved head, then, as the
   one final commit, the removal of `.legion/<issue>/`, so no merge carries a handoff onto the
   default branch (dispatch://LEGION-605); those commits do not void the approval and never return
   the tree to the tester or reviewer;
4. the merger quotes what sits above the reviewer-approved head (`jj diff --from <approved-sha> --to
   <tip-sha> --summary`) in READY and sends the READY packet with its completion, which refuses a
   head carrying anything above the approved head but retro's commits, those that change
   `docs/solutions/` and that removal (`READY_UNREVIEWED_CHANGE`), and a
   head that still carries `.legion/<issue>/`; the daemon posts
   `READY #<n> at <current sha> (approved at <approved sha>) for <KEY> (<pr url>)` on the Dispatch
   issue and publishes it to the project's merge queue role when one is set. The moment the daemon
   accepts READY, it submits the merge of that head itself, as the implement App and with GitHub's
   expected-head check: a merge queue enqueues it, a pull request already mergeable merges at once,
   a repository that allows auto-merge arms it, and in one that allows neither the daemon merges it
   once GitHub reports it mergeable. The repository's required reviews and checks decide when it
   lands, and Legion neither reads nor writes those rules; a review only a person can give is
   waited for, never bypassed, and once given the daemon merges or the person does. A READY the
   design gate refused is submitted by the daemon once that spec version is approved; nobody is
   told to submit it. If the merger reports a failed verification,
   treat it like `pr-blocked`: the merger holds the phase, so tell it to move the issue back with
   `request_backward_move`, naming what failed; never bypass. A READY refused
   `READY_UNREVIEWED_CHANGE`, or a merger task with no approved head (`READY_NOT_APPROVED`), goes
   back to `reviewing`, the move's reason naming the code and what it found, so the reviewer runs a
   new round. A READY refused because the head still carries `.legion/<issue>/` goes back to
   `retro`, so the implementer's retro removes it.
5. the merge lands under the repository's rules; the daemon then starts the **implementer** once
   more, on the production check.
   It drives the changed path in production through the user's own access path and records
   what it saw on the pull request and on this issue. Sign off only after the implementer's production
   report exists. A defect it finds is a corrective child issue of this tree, not a note on a
   closed one. If the implementer cannot perform the deploy itself, it asks the agent or role the
   deployment instructions name for that deploy (an Envoy role), gives it the merge commit, and
   waits on the production signal the instructions name; the issue waits with it. It opens a
   `dispatch ask` to a person only when the instructions name nobody for that deploy, starting
   with the production gap and why it matters, then naming the required step, its risk, and
   outcome-named options. No one is ever asked for approval to deploy.

What returns the tree to review: a changed diff — a commit above the approved head that
touches anything outside `docs/solutions/` and the issue's `.legion/<issue>/`, or a
conflict-resolution merge whose fingerprint
(the unchanged-diff check, `skill://legion-worker/references/conflicts-and-rewrites.md`) differs from the approved head's. What does not: retro's
`docs/solutions/` commit and its removal of `.legion/<issue>/`, and a merge forced by a GitHub-reported conflict whose fingerprint
is unchanged. For that merge, the worker holding the issue's phase moves it back to `implementing`
with `request_backward_move`, and the daemon runs the phases from there: the implementer merges
the bookmark forward with the
destination (the forward-merge procedure in `skill://legion-worker/references/conflicts-and-rewrites.md` — `jj new legion/<KEY> <destination>`,
never a rebase, since a rebase rewrites every descendant of the chain's fork point, including
another tree's branch stacked on it), pushes it as every role pushes (`jj git push`; a genuine
fast-forward, and a code push, so no `skip-checks: true` line), and posts the before/after fingerprints; the tester re-runs the bare gates only;
the reviewer confirms and approves the new head by SHA (or continues its round if it had not
approved); the daemon carries the issue on through retro to the merger, whose new READY packet the daemon posts.
This merge happens only when GitHub reports `CONFLICTING`
(`gh pr view <n> --json mergeable,mergeStateStatus`); read that on every end-game
wake — `phase-finished`, `catch-up`, `checks-red`, `review-stuck` — because a `CONFLICTING`
PR gets no CI and no wake announces it. The moment you see it, tell the worker holding the issue's
phase (`envoy_publish` to its role topic) to move the issue back to `implementing` with
`request_backward_move`; in `awaiting_merge`, where no worker holds a phase, open a `dispatch ask`
naming the conflict for the human who merges. Do not let the merger send a READY packet for an
obsolete approval.

If the implementer reports that GitHub refused to resolve a review thread — its own
`resolveReviewThread`, on a thread it answered or one the reviewer named as accepted — open a
`dispatch ask` that names the thread's id or URL and GitHub's message for a human to resolve it by
hand, with options for resolved / could not; the merger does not complete while it is open. That
is the one review-thread step a human takes: GitHub lets only the pull request author's App
resolve a thread, so the implementer resolves every thread with plain `gh`, the ones it answers
and the bot threads the reviewer accepted and named to it (an Envoy message to the implementer's
role topic, or its review body); the reviewer resolves none.

## 7. Close

After the merge result and the implementer's production report are recorded, post the sign-off and
close this issue with `sign_off`, which writes `done`:

```text
dispatch comment --issue LEGION-40 --body-file - <<'EOF'
<sign-off: scope, integration evidence, review, retro, merge, and the implementer's production report>
EOF
legion({ op: "sign_off", issue: "LEGION-40" })
```

Closing a child supplies the closure event (`child-closed`) to its parent. Do not close a parent
until the entire end-game sequence has completed.

## Wake routing

Handle one delivered wake by verifying the relevant live artifact and then performing the
corresponding lifecycle procedure. Architect-addressed wakes always reach the architect that owns
the payload issue: a claimed child sub-architect, otherwise the nearest claimed ancestor, then the
root. A wake about an architect role's own launch reaches the architect above it. `phase-finished`,
worker lifecycle, child lifecycle, and design-gate wakes are architect-only and never go to an
active phase worker.

| Wake | Procedure |
| --- | --- |
| `carried-gate-opened` | Only a root whose catch-up said `carried` gets it. Its `version` and `reason`: a human approved the old root's spec document at `version`, which opened the gate this root carried; the daemon has released any READY it refused and started any child waiting on it. It is not `design-approved`: do not proceed to section 2 or release a wave because of it, and start nothing. |
| `child-status` | A child of your tree left the workflow or re-entered it; the notice's reason names the child and its new status. `todo` (a human's move, your `release_children`, or your `rerun_child`) means the child runs again under your tree from planning, and the daemon starts it; you start nothing. `backlog`, `icebox` or `triage` (a human's move, or your `park_child`) means the daemon has suspended the child's workers, and it advances no further until it is set back to `todo`. What the rest of the tree does is your decision. |
| `child-closed` | Read the child completion and remaining open children. Re-scope open work, park obsolete work with `park_child`, and release an appropriate next wave with `release_children`. When a worker waits on this child for a missing surface (see `phase-finished`), tell it to continue with `envoy_publish` to its role topic. The last child's close is not the end-game (section 3). |
| `child-release-stalled` | Its `reason` names a child your `release_children` released while Dispatch already showed it `todo`, and that child's parent, whose release has not reached the daemon after three tries. Check the parent's record in `legion state`; run `release_children` for the child again once the parent is recorded in your tree, and tell the controller if it never is. |
| `design-approved` | Its `version` is the approved one. A human approved the root spec document at its current version; the gate is open. Proceed to section 2 — except on a root whose catch-up said `handed_over`: its workers already run their phases, so start nothing; the approval releases any READY the gate refused. One whose `version` is the version a human's own answer to a decision block wrote, after an earlier approval (Dispatch carried that approval to it, section 1), opens nothing new: carry on, and release no wave because of it. |
| `design-changes-requested` | Its `version` and `reason`. A human asked for changes to the root spec at `version`, for `reason`. Revise the spec as the reason asks and request approval again as section 1 says; stay parked; the gate is closed. |
| `phase-finished` | The daemon has already moved the issue to its next phase by its fixed table and started that phase's role; you start nothing. Read the committed handoff for the finishing phase; if it shows unresolved gaps, tell the role now working the issue (`envoy_publish` to its role topic). A `planner` notice that names a departure from the spec's design defers to section 1's full condition: when the approved Summary, Acceptance, scope and settled decisions still hold, the plan is the record; otherwise change the root spec and request approval again as section 1 says. A `reviewer` notice whose GitHub review is `CHANGES_REQUESTED` needs nothing from you: the daemon has returned the issue to `implementing` (Dispatch `in_progress`) and started the **implementer**, whose correction goes through the tester and the reviewer again, never straight to retro. A `reviewer` notice with an `APPROVED` review means the daemon has started retro (step 5). An `implementer` notice for `production_check` is its production report: read the record on the pull request and the issue, then run step 7. A `tester` notice with `verdict: "fail"` — its handoff carries `implementerProof.verdict: "rejected"`, or a failure naming the production-like proof — has gone back to the **implementer** by the daemon's table; never supply the proof from another role. A worker that reports no surface reaches the changed path sends that report instead of completing its phase, so the daemon starts nothing more on that issue and the worker stays idle in its session, not suspended: file a child issue in this tree to build the surface (infrastructure, tooling, or a skill), and when that child's `child-closed` arrives, tell the waiting worker to continue with `envoy_publish` to its role topic. That report is never a reason to advance the phase. |
| `pr-blocked` | Its `reason` names the pull request and the `max_fix_attempts` it reached, or reads `Issue reached review_round_cap=<N>.` (the end of this row). The count is of heads pushed onto a red verdict that changed something outside `.legion/` — handoff-only pushes (`.legion/` paths only) never count, a push by the review App (a planner's, tester's, reviewer's or architect's) never counts, and the head after a red the tester's red tests earned (a review-App push that changed a path outside `.legion/`, however many handoff-only pushes follow it) does not count either — so after the tester's handoff-only push onto the implementer's red, the implementer's next push does count; a push the daemon cannot classify (a listener without `changed_paths`, a list the listener stopped at 100 paths or 32,768 runes of text, a push listing no commits) does. Published once per exhausted count, not on every later red verdict for that count. Read the failed CI evidence and recovery attempts. The notice moves nothing, and no later CI red within the attempt that ran the count out (the counted head, and every head pushed on top of it until a push lands on a head CI judged green, the tester's red tests and handoff onto the held red included) sends the work back from `testing`, `reviewing`, `retro` or `merging` either: the issue stays in its phase until you move it, or a worker's own decision does (a tester's `fail`, a reviewer's request for changes). A head pushed onto green CI leaves that attempt, so its red still comes as a `checks-red`. In `implementing`, give the implementer the failing checks (`envoy_publish` to its role topic). In any later phase a worker holds, tell that worker (`envoy_publish` to its role topic) to move the issue back to `implementing` with `request_backward_move`, naming the failing checks, and the daemon starts the implementer; or file a corrective child. In `awaiting_merge`, where no worker holds a phase and a backward move is refused, open a `dispatch ask` naming the failing checks for the human who merges, as section 6 does for a conflict there. Do not treat the blocked PR as final. A `review_round_cap` reason is the implementer's round count instead: every move back to an earlier phase counts one, and the notice comes again with each round past the cap. It moves nothing either, since the round that reached the cap has already started its worker. Tell the rounds apart by what announced each: a tester's fail by its `phase-finished` with `verdict: "fail"`; a request for changes by the reviewer's `phase-finished` over a `CHANGES_REQUESTED` review; the daemon's own move by a `checks-red` notice, whose reason names a conflict (`the head conflicts with <base>`), the required checks that failed (`CI is red at <head>`), or the merge queue's failed merge and its failing run (`the merge queue took pull request #<n> out at READY's head …`); a worker's `request_backward_move` by a `phase-finished` from the role that asked, whose reason reaches only the task of the worker the move started, so ask that worker. Judge the cap by the rounds that are not conflicts. Conflict returns need nothing from you beyond the next round. Tester fails, requests for changes and CI-red returns (a `checks-red` notice is raised for CI only when a required check fails on code the branch pushed, or when the merge queue fails the merge) are the work going in circles: narrow it, re-plan it with the responsible worker, or ask a human with `dispatch ask`. Judge a worker's backward move by the reason that worker gives. A move you ordered for a conflict found before `awaiting_merge` (section 6) is a conflict return. The merger's move back to `reviewing` for a READY refusal (`READY_UNREVIEWED_CHANGE`, or `READY_NOT_APPROVED` when its task names no approved head), and the reviewer's no-fix request for changes that follows it so the tester runs the head, are not the work going in circles either. A move that sends back code found wrong counts with the tester fails. |
| `pr-merged` | Its `reason` names the pull request. The PR merged, under the repository's rules, before the issue reached `awaiting_merge`. The workflow runs on and asks no one to merge it: the daemon starts the **implementer** on the production check once the issue gets there, and you start nothing. Its `phase-finished` for `production_check` is what brings you to step 7: verify the record on the pull request and this issue first, then sign off naming it with `sign_off`. A merge is not the close. |
| `pr-closed-unmerged` | Decide from current scope whether the work is reopened, started over (`park_child` then `rerun_child`, for a child), or ended with a reason. Delegate the repository action to the responsible phase worker and keep ownership. |
| `merge-refused` | Its `reason` names the pull request, READY's head, what the daemon asked GitHub to do (merge, enqueue, or enable auto-merge for it), and GitHub's refusal, which a retry would meet again (squash merges disabled, a permission the implement App lacks). The daemon has stopped submitting that merge; the issue stays in `awaiting_merge`. The cause is a repository setting or an App permission, a person's to change: ask one with `dispatch ask`, naming the refusal and the pull request at READY's head, which that person merges under the repository's rules once it is fixed. The merge moves the issue on to its production check. Never bypass a rule. |
| `merge-stalled` | Its `reason` names the pull request, the state and head GitHub shows (closed, or another head), since when GitHub has shown it, and READY's head, which the record still awaits. GitHub's last change to the pull request is more than 10 minutes old and the record never heard of it, so its webhook never reached the daemon: the daemon has stopped submitting that merge, and the issue stays in `awaiting_merge`. Read the pull request on GitHub. Open at READY's head, GitHub has caught up: a person merges it under the repository's rules, and the merge moves the issue on to its production check. Closed, or at another head the daemon never recorded: ask a person with `dispatch ask`, naming the pull request, what GitHub shows, and READY's head. Never bypass a rule. |
| `merge-removed` | Its `reason` names who took the pull request out of the merge queue or turned its auto-merge off at READY's head after the daemon submitted it, or says GitHub turned it off on its own (over a repository setting such as a merge method the repository blocks) and names the App that submitted the merge, which GitHub names as the actor of its own disables; then what was removed, GitHub's reason (`manual`, `merge_conflict`, `manually_disabled`, `merge_commit_blocked`, …), when, and READY's head. It was not a failed merge, which comes as a `checks-red` instead. The daemon submits that merge no further, so the removal stands, and the issue stays in `awaiting_merge`. Read the pull request on GitHub. A `merge_conflict` conflicts with the base or with a pull request queued ahead of it: once the conflict is on the base, the daemon's own conflict check sends the issue back to `implementing` by itself. Otherwise ask a person with `dispatch ask` (the one who removed it, when the reason names a person rather than GitHub on its own), naming the pull request at READY's head and the reason; that person merges or queues it again under the repository's rules, and the merge moves the issue on to its production check. Never bypass a rule. |
| A reply on an ask you follow | Interpret the reply in the issue's design context. Answer in its thread (`dispatch comment --reply-to <comment id>`; under an open ask whose next move is yours, such as the approval request you must revise or hand back, `--reply-to-ask <ask id> --turn agent`, since a default-turn reply hands that request back to the human and a corrected `--summary` is then refused), then adjust the plan or relay it via `envoy_publish` to the responsible worker's role token; scope and product decisions remain with you. |
| `worker-died` | Its `role` and `phase`. That role's claim failed: its launches or prompts ran out. For a phase worker the daemon holds the issue (phase `held`) and starts nothing more on it. Reassess the work, then decide with `retry_or_escalate`: `retry` when the failure looks agent-specific or transient, `escalate` to hand the held issue to the controller when it looks environmental. |
| `held` | Its `role` and `phase` (the phase the issue left), or `phase` with `reason: "escalated"`. Without `reason`, that phase's worker ran out of launches or prompts: the issue is held (phase `held`), the `worker-died` that comes with it is yours to answer with `retry_or_escalate` as that row says, and the controller hears of the hold too. With `reason: "escalated"`, it records your own `escalate`: the controller has the issue now, and you start nothing for it. |
| `ready-refused` | Its `version` and `reason`. The merger's READY was refused, and its packet is kept with the completion so no second READY is ever needed. With `version` greater than 0, `reason` reads `READY refused: approve design version <N> before requesting READY.`: the root's spec is already registered at that version, so get it approved (section 1), and the daemon advances the merge and posts the kept packet the moment it is. With `version` 0, `reason` reads `READY refused: no design version is approved; register and approve the tree's spec before requesting READY.`: the root has no spec registered yet, so register it and get it approved (section 1) exactly as above; the daemon releases the kept packet the same way — at registration itself when that opens the gate (`gates.design: off`, or Dispatch already shows the version approved), otherwise the moment a human approves the version you registered. Either way, nothing else is needed once the gate opens. On a root whose catch-up said `carried`, the gate is a copy on the old root's spec document: run nothing of section 1; tell the controller the READY waits on a human's approval of that document, and `carried-gate-opened` follows the approval. A `reason` starting `READY_PACKET_MISSING` names a READY refused before the daemon kept packets: that READY is void and the issue stays in merging, so start it over (`park_child` then `rerun_child`, for a child) or end it. |

## Escalation judgment

Controller-actionable matters are exactly re-filing a genuinely independent child, capacity, and
cross-tree conflict. Report those to the controller with `envoy_publish` to the controller topic
your `Legion addressing` line names. Handle everything else in the
tree. A product, scope, or design decision that needs the human, yours or one a worker escalated,
is a decision block you write (section 1 says what one does to the root spec's gate). A standalone
human to-do may use `dispatch ask`; workers may reach the human directly with it the same way. Do not
create a wait loop for any wake source.

Never yield while waiting on a human. A human is waiting on you only where an open ask sits in
their inbox, so open it before you stop: a decision block in the spec, `dispatch ask` for a
standalone human to-do, or `dispatch request-approval` for the spec gate. Otherwise proceed:
proceeding is the default, and a stop that waits on nobody stalls the tree until someone notices.

## Architecture components

Bootstrap on a root whose project has an architecture source: in the root's first PR (the implement worker pushes it), write `.dispatch/architecture/<id>.md` files for the planned components — front matter `title`, `parent`, `depends_on`, `external`; no `paths` yet. The importer reads the source branch's head, so the sync and the attach below work only once that PR has merged to the source branch: until then leave the root on `inherit` (the attach would answer `400 COMPONENTS_INPUT`, the component does not exist yet). After the merge, run `dispatch architecture-sync --project <KEY>` and attach the root with `dispatch issue-update --issue <root> --components-json '{"mode":"explicit","ids":[…]}'`. Children inherit the root's attachment; give a child its own `--components-json` only when it changes a narrower set, and `{"mode":"none","reason":"…"}` when it is not architectural work. Attach before decomposing, and require every implementer to change the component file beside the code it describes in the same review.
