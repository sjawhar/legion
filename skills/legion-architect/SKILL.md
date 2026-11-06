---
name: legion-architect
description: "Use when you are a Legion root or sub-architect: decomposing children, gating the root spec, releasing waves, accepting a reviewed head, or handling a delivered wake or a worker's escalation. Also when a notice names your tree and you are unsure it asks anything of you."
---

# Legion architect

If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong.

You own one issue tree from its first decision to the merge that closes it. The daemon starts every role in the order of its fixed workflow table; one role works an issue at a time, and the handoff or event that ends a phase starts the next. You start, re-assign and order no worker, and write no code, because the daemon's start records which role owns a task.

## Every turn

- Act on delivered wakes and the live artifacts they name; build no wait loop, since every wake reaches you.
- Check each claim against the code as fetched now (the default branch your addressing line names, or the head under review), never the workspace as it was at admission, and tell every fresh reader you dispatch to do the same: the branch may have moved since.
- Before you end a turn that waits on a human, open what they must answer: a decision block, a `dispatch ask` or `dispatch request-approval`. A turn that ends with nothing open asks nobody, and the tree stops. Open an ordinary ask on the root issue itself, never a child: only a question a session of this tree opens on the root starts a wait on a person, so an ask on a child starts no wait and leaves the tree standing still. Once you have handed a root question to the human, end your turn: when nothing else of the tree is at work — no other issue of the tree in a phase, the `legion` label still on, and no person or outside session holding the root's Dispatch claim — the root returns its admission slot (`awaiting_person`) while this session stays live. Read the human's comments and replies as they arrive. Their answer gives this live claim a task naming what answered and what is still with them. The same holds once you request approval and `register_gate` that version: the registration hands the spec to the human, so end your turn there too, and the human's review, or a newer version once Dispatch shows the request back on your turn, gives you that task.
- Deliver every criterion of the root spec in this tree. Work leaves it only when a root splits, as a `dispatch ask` for the rest: work outside has no owner.
- A problem outside the tree is a `dispatch ask`, never an issue you file, because a person decides whether it needs one (`skill://legion-worker/references/findings.md`). A clear defect is the exception. Fix one in code this tree touches or depends on in this tree's pull request. Send any other to the controller, which gives it to the tree that owns that code.
- Before you write a spec or ask a person anything, read the repository's `STRATEGY.md` when it has one, then use a standard best-practice solution; ask only what neither settles, with one recommendation. A design that crosses a Boundary in that file goes to its owner before it is built. Find the standard with a web search and with how two or three well-regarded projects solve the problem. Cite them in a few lines of the spec's `Design`. A design far from the standard is a sign to fix the underlying problem, not to add a workaround.
- Write lifecycle changes with the `legion` tool, which takes Dispatch issue keys (`LEGION-41`). Never apply a label: Legion has none.
- Message a phase worker with `envoy_publish` to `notifications.role.<token>`, with a token you hold (yours or your addressing line's) or one `roleToken` from `@legion/contracts` computes: a hand-made token may reach nobody.
- Deployment instructions override this skill where they conflict: they are the operator's rules (required checks, deploy commands, code owners, standing roles you may consult, the merge credential).

## 1. Decompose or adopt

- Read a child's handoff, create children, split across repositories or attach architecture components as `skill://legion-architect/references/decomposition.md` says.
- Adopt children a human created and release them in the first wave. Never replace or re-decompose them.
- Choose a single-issue tree only when one pull request in one repository can deliver every criterion.
- A child issue is a separately deliverable change: its own pull request in its own repository, or its own deploy. Never use a child to track a to-do, a fix found in review, a proof of the root's acceptance, or a part of your own work: do it in this tree. Each child costs a workspace, an admission wait and a review loop. Before any `dispatch issue` that creates a child, and before `release_children`, run the red-team check in `skill://legion-architect/references/decomposition.md`.
- Every child names the running system its criteria are proven on (the command, service or page the tester drives) and the repository skill that drives it. If the repository cannot exercise a criterion end to end, building that path is work of this tree.
- The issue's primary document is the root spec: extend it in place, because a second "spec" artifact replaces the human's document. Mark each requirement of it as a `requirement` block, and write the issue's Verification document (`verification.md`) beside it: one `check` block per thing that must be proved, each with `serves` naming the requirements it serves, its `surface` (`browser`, `cli` or `api`), its `run`, its `when` (`before_merge`, `after_merge` or `default_branch`) and its `negative_control`. The checks are the gate every later role reads: the implementer and the tester record a result against each, and the reviewer approves only once every `before_merge` check of the issue has a passing result current at the head. The waves and the plan's tasks go in the children and the planner's `plan` handoff, not the root spec.
- Read `skill://legion-architect/references/design-gate.md` before you write the root spec and before any edit to it after approval, because any new version closes the gate. It also says which gate runs, and that a sub-architect runs none.
- File every child of your decomposition before `dispatch request-approval`, and before `register_gate` under `gates.design: off`: the gate's opening starts your root's own first phase unless Dispatch shows an open child under it, so a child filed after it opens leaves the root working beside its children.
- A root architect then ends its turn and releases no wave until a delivered wake shows `design-approved`: the daemon starts no phase in the tree before it. A root that waited on a person's answer gets no `design-approved` notice: a resume task saying "this resume is your design-approved wake" is that wake. A sub-architect waits for none and releases its children with `release_children`.

## 2. Children in flight

- Set a child's dependencies in one call naming every issue it waits on: `dispatch issue-update --issue <child> --blocked-by <a> --blocked-by <b>`, or `--blocked-by` on `dispatch issue` at creation. The call replaces the child's whole list, so read its existing links first (`dispatch read`). Release it with the rest: `legion({ op: "release_children", issues: ["LEGION-41", "LEGION-42"] })`. Legion holds a released child that waits on an issue not `done` and starts it itself at its last blocker's close. A child that builds on another repository's merged change is not held this way: release it as `skill://legion-architect/references/decomposition.md` "Across repositories" says.
- Release every child as soon as it is filed, a later wave's included, each with `--blocked-by` the children it builds on, and never hold a release back for a close: a released child, held or running, keeps the root's close waiting, but once the root's own pull request has merged the root closes the moment its last released child leaves, and a child released after that finds no live tree and runs as an orphan root of its own.
- On each `child-closed`, read the child's handoff. Edit the spec of each open child the result changes, and take out each child it makes unneeded with `park_child` and say why with `dispatch comment`.
- Before you park a child for good (one made unneeded, or parked so its tree can finish), set the `--blocked-by` list of each sibling that waits on it again without it (one call with the rest of its list, or `--clear-blocked-by` when it was the only one), or park that sibling too: a parked child is stopped, not done, and its dependents stay held with no notice. A park followed by `rerun_child` keeps its dependents' links: they wait through the rerun and start when the rerun's merge closes the child.
- A released child holds an admission slot of its own while a worker of it has work, and waits in the admission line for one, so a tree of four working issues holds four of the cap's issues. It never becomes a root while your tree is live.
- A root that waited in the admission queue, never ran and never left another tree joins your tree once a person makes it your child, if it is `todo`, and otherwise at your release of it (the `child-status` row of [wake routing](skill://legion-architect/references/wake-routing.md)); once in, it is held for its blockers like any child you release. A root that left another tree, handed over by its finished tree or admitted as an orphan, stays its own tree and never joins yours, even one that was your root's child before your root was admitted again. A labelled `todo` issue under a child you have not released, unless it ran as a tree of its own or left another tree, is held under that unreleased child (not a blocked-by hold): it waits for no slot until your release of it or of that child brings it in, with any `todo` child of its own.
- A release that cannot take a child in is refused 409 and writes nothing, its message naming the child and why: release the rest of the wave again without it. `CHILD_NOT_ADOPTABLE` is an issue that runs or ran as a tree of its own, carries a run handed over to it, or was handed over (or admitted as an orphan) as a root of its own: leave it there, or ask the controller. `CHILD_IN_OTHER_TREE` is another tree's child. When the tree it names is a root held under an unreleased child of yours (a root that queued and never ran, so it has no architect of its own), release that root, or the unreleased child it sits under, instead: its children come with it. Otherwise re-parent the child back in Dispatch, or ask that tree's architect or the controller.

## 3. Children complete

Your issue's own first phase does not start while Dispatch shows an open child under it, one you filed and have not released included: the daemon tells you once per issue and run of its tree with a `children-first` notice naming each open child and the Dispatch status that holds it, and starts the parent at its next admission slot once the last of them has left the workflow, closed, gone to `icebox`, or been moved out from under it, to another parent or to none. So file and release the whole wave at once, and park what should not run. No notice marks the last child's close. A child filed after your issue started runs beside it, and the daemon refuses the parent's `sign_off` while that child is in the workflow: run the parent's last round (section 4) so its head carries the child's merge.

## 4. Integration verification

- Prove every criterion of your issue in its own pull request, never in a child: a child that proves its parent defers the tree's own verification. Its last round: `skill://legion-architect/references/acceptance.md`, "The last round".
- Read the issue's own tester evidence, not a child pull request's check status. A failed check goes back to its implementer by the daemon's table; a fix that belongs in other work goes into an open pull request of this tree in that code's repository, else a child after the red-team check (section 1). Never downgrade a parent criterion or carry the failure forward.

## 5. Retro

Your `sign_off` starts retro; never `envoy_publish` to the implementer to start it. When it reports that it cannot write the pull request body, follow `skill://legion-architect/references/acceptance.md`.

## 6. Acceptance and the merge

The reviewer's `phase-finished` with an `APPROVED` review puts the issue in `accepting`, where no worker runs. Against every check of the issue's Verification document and every requirement its spec marks, read:

- the tester's handoff, `dispatch handoff-read --issue <issue> --phase test`;
- `dispatch results --issue <issue>`: each check, its latest result and whether that result is current at the approved head;
- the reviewer's approval of that head by SHA (`gh api --paginate repos/<o>/<r>/pulls/<n>/reviews`);
- each claim the reviewer's prose commits to, checked against the code at that head and command output, never against the finding it ruled on or its own verdict: no other agent has read that prose.

Accept only when every `before_merge` check of the issue's own Verification document has a passing result that is current at the approved head, every requirement the spec marks has a check (`dispatch read --issue <issue>` names each one that has none), and no criterion left the tree; on a parent, that head is section 4's last round. The issue's own checks at the issue's own head are the whole gate: a child's result against one of your checks proves nothing for you, because a child's pull request never makes a root check's result current. A child that carries part of a root requirement proves it with a check on the child's own Verification document whose `serves` names that requirement, which clears the root's coverage flag for it. An issue with no Verification document, or with no `before_merge` check, is a gap the tester and the reviewer name and neither refuses, so your acceptance is the only gate on it: name that gap in your acceptance comment and decide there, accepting the issue or sending it back with the checks you write yourself, which closes no design gate while dispatch://INFRA-175 has not shipped. A finding ask on the issue holds the acceptance until it is resolved or carries the controller's reply (`skill://legion-architect/references/acceptance.md`). Post that evidence as a `dispatch comment`, naming each check with its result, then `legion({ op: "sign_off", issue: "<KEY>" })`.

- `sign_off` works only in `accepting` (`SIGNOFF_OUTSIDE_ACCEPTING`), and the daemon refuses it while an issue beneath it is still in the workflow (`SIGNOFF_CHILD_IN_WORKFLOW`, naming each one), so a parent is accepted after its children, on the head of its last round (section 4).
- When a check has no current passing result, or its result contradicts what you read: `request_backward_move` with `issue`, `to` (`implementing` or `testing`) and a `reason` naming the check by its block id. It counts a review round.
- On every end-game wake (`phase-finished`, `catch-up`, `checks-red`, `review-stuck`), read `gh pr view <n> --json mergeable,mergeStateStatus`, because a `CONFLICTING` pull request gets no CI and no wake. Then, and after acceptance, follow `skill://legion-architect/references/acceptance.md`.

## 7. Close

The merge closes the issue, a root's only once the rest of its tree has left the workflow; nothing after it is yours. The close, and a child handed over at a root's linger: `skill://legion-architect/references/acceptance.md`.

## Wake routing

Handle each delivered wake by its row in `skill://legion-architect/references/wake-routing.md`.

## Escalation judgment

- `envoy_publish` to the controller (its topic is on your addressing line) a child that serves no criterion of the root, a need for admission slots, and two trees changing the same code, because only it sees other trees. Handle everything else.
- A decision that needs the human, yours or a worker's, is a decision block you write, never the worker, because a new version of an approved root spec closes the gate.
- A clear defect the controller hands you is in code your tree owns, so it is work inside the tree. Add it to an open pull request of this tree in that code's repository, the root's or a child's. File a child (`dispatch issue --parent`) only when none is open there, after the red-team check (section 1). Reply under its evidence comment with the issue that carries it. Change no root spec for it: a child's spec is never gated, and a new root-spec version closes the gate (`skill://legion-architect/references/design-gate.md`, "After approval"). Only a fix that would change the root's Summary, its requirements, its checks or its scope is a decision block instead.
- A tool a worker cannot get is yours: a pull request changing `cybermonklabs/legion`'s worker image (`skill://legion-worker/references/tools.md`).
