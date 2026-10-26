# The root spec and the design gate

Part of `skill://legion-architect`, section 1. Read it before you write the root spec, before any
edit to an approved root spec, and on `design-approved`, `design-changes-requested` or
`ready-refused`.

## Writing the root spec

Specifications written into Dispatch follow
[`dispatch-writing-specs`](skill://dispatch-writing-specs/SKILL.md#the-outline). Wave releases, child closures and your
own status are visible from the issue tree and the handoffs; do not narrate them into the spec or a
`dispatch message`.

Extend the primary document in place: a new version adds only the evidence each decision needs and
what the human decides, each as a [decision block](skill://dispatch/SKILL.md#asks-and-ask-blocks).
Never post a second "spec" artifact beside it: `dispatch artifact` with the primary document's name
replaces the human's document.

## Which gate runs

The approval step runs only when the `Design gate policy` sentence of your `Legion addressing` line
says `gates.design: root-issues`. When it says `gates.design: off`, write the spec and register it
with `register_gate` at its current version, with no approval step: do not request approval and do
not wait for `design-approved`; the gate opens at registration. A sub-architect on a child issue
has no policy line and never runs the gate.

## The armed gate

When the gate is armed, run this exact sequence before the tree's work starts:

1. Extend the primary document in place:

   ```bash
   dispatch doc-edit --issue <root issue> --ops-json-file - <<'EOF'
   ...
   EOF
   ```

2. Settle every decision block as
   [Approval](skill://dispatch-writing-specs/SKILL.md#approval) says;
   `dispatch request-approval` refuses while one is open. Each answer reaches you, since you follow
   every ask you open.
3. Before every approval request, give two fresh reviewers the current root spec, the code and
   command output it relies on, and the human's words: `task(agent="plan-gap-analyst")` finds its
   gaps, and `task(agent="plan-reviewer")`, told the document is a spec, checks each factual claim
   against the code, the docs or a read-only command it runs. Resolve every finding in the root
   spec. Check with a command each claim either reviewer marks unverified: fix it when it is
   false, or take it out.
4. Request approval only once the human has agreed to every point in the spec, because the human
   approves what they already agreed to and a point new in the request has no decision block for
   them to answer: give such a point its own decision block first, or take it out of the spec.

   ```bash
   dispatch request-approval --issue <root issue> --summary '<what the human is approving> Reviews: <gap result>, <claim result>.'
   ```

   `--summary` says in one to three sentences what the human is approving and nothing else (no
   commentary and no open question), then one sentence with both reviews' results, for example
   `Reviews: no open gap, 14 of 14 claims verified.` The primary document is the default; the
   command prints `Approval requested for spec.md (document id <UUID>) at version <N> (ask <id>)`.
5. Register that document version with the daemon:

   ```ts
   legion({
     op: "register_gate",
     issue: "<root issue>",
     artifactId: "<UUID>", // the document id the request printed
     version: <N>, // the version the human is asked to approve
   })
   ```

   Copy `artifactId` and `version` from what `dispatch request-approval` printed. The document id is
   never the slug or file name you passed in (`spec`, `spec.md`): the daemon recognizes the
   document's approval events by that id alone; the `legion` tool looks a slug or file name up in
   Dispatch and hands the daemon the id.

Facts about the request:

- `dispatch request-approval` opens a system question with the fixed options `Approve` and
  `Request changes`; a human answers it from the Inbox or approves from the document's header.
  Never open a `dispatch ask` with an `Approve` option yourself: the daemon ignores its answer.
- Repeating the request while it waits on the human, with the same `--summary`, changes nothing and
  returns it ("already waits on the human"); a different `--summary` is refused then.
- A newer version of the spec moves the open request to that version and leaves it waiting on you.
  No wake tells you when the edit was yours. Once the human has agreed to every point in it, call
  again to hand the same request back at the latest version.
- If the output reads `spec.md (document id <UUID>) is already approved at version <N>`, still call
  `register_gate` with that id and version: the daemon reads the approval from Dispatch as it
  registers, opens the gate and delivers `design-approved` at once, and only once even when the
  approval's own event opened the gate first. The same read covers a human who answers between
  your two calls: a changes request on that version arrives as `design-changes-requested`, once.
  You never approve anything yourself.
- `register_gate` refused 409 `VERSION_NOT_IN_DISPATCH` means the version you named is above the
  latest version Dispatch holds for that document (the message names both), and a version Dispatch
  never wrote is no registration: the gate is as it was. Read the document's latest version, check
  it is the spec the human is asked to approve, and `register_gate` that version.

Then end your turn. Release no wave until a delivered wake shows `design-approved` on the root. On
`design-changes-requested`: revise the spec as the human's reason asks, run step 3's two reviews on
the new version, resolve every finding, request approval again and end your turn. A
`design-changes-requested` may name a version below the latest, or arrive after another for the
same version: it is a request the human made that reached the daemon late, and no approval came
after it. Each request arrives once. The gate is as the newest review left it; still read the
reason, and answer it in the spec if the latest version does not already.

## After approval

Approval is pinned to the spec version: any new version closes the gate again, and no
`design-approved`-type wake says so; you know from your own edit, or from the `artifact.version`
event on your issue. The exception is the
version a human's own answer to a decision block writes when that answer is the whole change since
the version they approved: Dispatch records an `approved` review in their name at the new version
and emits `artifact.approved`, and the daemon reopens the gate and sends `design-approved`.

So edit an approved root spec only when its Summary, its Acceptance, the tree's scope, or a
decision a human settled in one of its decision blocks changes. Such a change is a point the human
has not agreed to: put the problem behind it to them as its own decision block, with its evidence,
at the end of the section it changes, and add nothing else to the spec. If you edit the block before
the human answers, the gate closes like any new version, but their answer still carries the approval
to the version it writes. Once they have answered:

- When the answer is the whole change, request nothing: the document's chip reads Approved at the
  new version, its reviews dialog shows the review in their name, and the `design-approved` the
  daemon sent opens nothing new. Carry on, and release no wave because of it.
- When the answer changes the Summary, the Acceptance or the tree's scope, rewrite that text and
  repeat steps 3 to 5 on the new version, with a one-sentence `--summary` naming what changed since
  the earlier approval, then the reviews' sentence (the card shows that approval, `Approved at
  version N on <date> by <who>.`, and its `Show diff` control carries the detail). The rewrite
  closes the gate until they approve;
  work the daemon started at the answer keeps running. Release no new wave until the `design-approved`
  after the rewrite; work already in flight continues. Every merger's `READY` in the tree is
  refused until a human approves the latest version.

A settled decision is the human's. A plan that would overturn one goes back to the planner with the
decision kept, unless the planner brings evidence the human did not weigh that would change it
(such as a measurement showing the settled choice cannot meet the Acceptance); then a decision
block names the decision and that evidence. A plan never overturns a settled decision on its own.

A design change that leaves the Summary, Acceptance, scope and settled decisions intact (a
planner's measurement that finds a better way to build the same outcome) goes in the plan (the
issue's `plan.md` document and `.legion/<issue>/plan.json`), never into the approved spec, even
where the spec's text describes the older design; the reviewer reads the plan beside the spec.

When a planner's `phase-finished` names a departure from the spec's design, compare it with the
approved spec. If the Summary, the Acceptance, the scope and every settled decision are unchanged,
the plan is the record and the tree carries on; otherwise change the root spec and request
approval again.

Later waves, re-scoping open children toward the same Acceptance, and integration-failure children
need no spec edit and no new approval. A child issue's spec is never gated.

## A decision block for a read no pod can make

An acceptance criterion sometimes needs evidence from a production or external system before a
human can approve it: a measurement, a current count, a stored record. First check whether a
credential this tree's pods carry reaches it: the model route every pod has, and any identity the
operator's deployment grants pods. Read the pod's own environment (for example `AWS_CONFIG_FILE` or
`AGENT_SECRETS_URL`), because a credential can exist without any skill naming it.

When the evidence is not reachable, do not have another running session read it for this tree and
fold the result into the spec, because that hides the gap from the next tree that hits it. Write
the missing capability as its own decision block to the human (which external system, which read or
write, why the spec needs it), and leave it open until the human resolves it.
