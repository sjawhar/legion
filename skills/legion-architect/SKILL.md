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
- Before you end a turn that waits on a human, open what they must answer: a decision block, a `dispatch ask` or `dispatch request-approval`. A turn that ends with nothing open asks nobody, and the tree stops.
- Deliver every criterion of the root spec in this tree. The only way to move work out is a child issue you create and own, or, when a root splits, a `dispatch ask` for the rest: work outside has no owner.
- A problem outside the tree is a `dispatch ask`, never an issue you file, because a person decides whether it needs one (`skill://legion-worker/references/findings.md`).
- Write lifecycle changes with the `legion` tool, which takes Dispatch issue keys (`LEGION-41`). Never apply a label: Legion has none.
- Message a phase worker with `envoy_publish` to `notifications.role.<token>`, with a token you hold (yours or your addressing line's) or one `roleToken` from `@legion/contracts` computes: a hand-made token may reach nobody.
- Deployment instructions override this skill where they conflict: they are the operator's rules (required checks, deploy commands, code owners, standing roles you may consult, the merge credential).

## 1. Decompose or adopt

- Read a child's handoff, create children, split across repositories or attach architecture components as `skill://legion-architect/references/decomposition.md` says.
- Adopt children a human created and release them in the first wave. Never replace or re-decompose them.
- Choose a single-issue tree only when one pull request in one repository can deliver every criterion.
- Every child names the running system its criteria are proven on (the command, service or page the tester drives) and the repository skill that drives it. If the repository cannot exercise a criterion end to end, building that path is a child of this tree.
- The issue's primary document is the root spec: extend it in place, because a second "spec" artifact replaces the human's document. The waves, the proof of each outcome and the integration test go in the children and `.legion/<issue>/plan.json`, not the root spec.
- Read `skill://legion-architect/references/design-gate.md` before you write the root spec and before any edit to it after approval, because any new version closes the gate. It also says which gate runs, and that a sub-architect runs none.
- A root architect then ends its turn and releases no wave until a delivered wake shows `design-approved`: the daemon starts no phase in the tree before it. A sub-architect waits for none and releases its children with `release_children`.

## 2. Children in flight

- Release a child only after each child it depends on has closed, because Legion does not order children: `legion({ op: "release_children", issues: ["LEGION-41", "LEGION-42"] })`.
- On each `child-closed`, read the child's handoff. Edit the spec of each open child the result changes, take out each child it makes unneeded with `park_child` and say why with `dispatch comment`, and release each child that can now start.
- A released child runs in your tree's admission slot, never its own, and never becomes a root while your tree is live.
- A root that waited in the admission queue and never ran joins your tree once a person makes it your child, if it is `todo`, and otherwise at your release of it (the `child-status` row of [wake routing](skill://legion-architect/references/wake-routing.md)). A labelled `todo` issue under a child you have not released, unless it ran as a tree of its own, waits for no slot until your release of it or of that child brings it in, with any `todo` child of its own.
- A release that cannot take a child in is refused 409 and writes nothing, its message naming the child and why: release the rest of the wave again without it. `CHILD_NOT_ADOPTABLE` is an issue that runs or ran as a tree of its own (or carries a run handed over to it): leave it there, or ask the controller. `CHILD_IN_OTHER_TREE` is another tree's child: re-parent it back in Dispatch, or ask that tree's architect or the controller.

## 3. Children complete

No notice marks the last child's close. The root can reach `accepting` before any child merges, but its `sign_off` waits for them (section 6).

## 4. Integration verification

- File a final integration child whose acceptance is every parent criterion proven on the default branch your addressing line names (never a hard-coded `main`), in each repository a criterion spans. Release it only after every other child has closed, because it must test a branch that carries their merges; accept the root only after it closes.
- Read its tester evidence, not a child pull request's check status. A failed check goes back to its implementer by the daemon's table; when the fix belongs in other work, create and release a corrective child wave. Never downgrade a parent criterion or carry the failure forward.

## 5. Retro

Your `sign_off` starts retro; never `envoy_publish` to the implementer to start it. When it reports that it cannot write the pull request body, follow `skill://legion-architect/references/acceptance.md`.

## 6. Acceptance and the merge

The reviewer's `phase-finished` with an `APPROVED` review puts the issue in `accepting`, where no worker runs. Against every acceptance line of the spec and the plan, read:

- the tester's handoff, `.legion/<issue>/test.json` at the approved head;
- the pull request body's `E2E (implementer)` and `E2E (tester)` lines;
- the reviewer's approval of that head by SHA (`gh api --paginate repos/<o>/<r>/pulls/<n>/reviews`);
- each of the reviewer's prose findings, checked against the code at that head and command output, not against the reviewer's own verdict: no other agent has read that prose.

Accept only when each acceptance line has passing evidence at the approved head, every corrective child and (for a root) the integration child has closed, and no criterion left the tree. Post that evidence as a `dispatch comment`, then `legion({ op: "sign_off", issue: "<KEY>" })`.

- `sign_off` works only in `accepting` (`SIGNOFF_OUTSIDE_ACCEPTING`) and is refused 409 `SIGNOFF_CHILD_IN_WORKFLOW` while any issue beneath this one is in the workflow, naming each: wait for the last, or take one out with `park_child`.
- When an acceptance check fails: `request_backward_move` with `issue`, `to` (`implementing` or `testing`) and a `reason` naming the failure. It counts a review round.
- On every end-game wake (`phase-finished`, `catch-up`, `checks-red`, `review-stuck`), read `gh pr view <n> --json mergeable,mergeStateStatus`, because a `CONFLICTING` pull request gets no CI and no wake. Then, and after acceptance, follow `skill://legion-architect/references/acceptance.md`.

## 7. Close

The merge closes the issue, a root's only once the rest of its tree has left the workflow; nothing after it is yours. The close, and a child handed over at a root's linger: `skill://legion-architect/references/acceptance.md`.

## Wake routing

Handle each delivered wake by its row in `skill://legion-architect/references/wake-routing.md`.

## Escalation judgment

- `envoy_publish` to the controller (its topic is on your addressing line) a child that serves no criterion of the root, a need for admission slots, and two trees changing the same code, because only it sees other trees. Handle everything else.
- A decision that needs the human, yours or a worker's, is a decision block you write, never the worker, because a new version of an approved root spec closes the gate.
- A tool a worker cannot get is yours: a pull request changing `sjawhar/legion`'s worker image (`skill://legion-worker/references/tools.md`).
