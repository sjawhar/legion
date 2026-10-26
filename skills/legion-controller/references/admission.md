# Reading admission

`read_state` says where each issue Legion records stands, and its `phase` alone does not: an issue
is `admitted` from the moment the daemon records it until its first phase starts, whether it runs,
waits, or neither. Read its key against `admission` and its tree's root.

| What `read_state` shows | What it means | What you do |
|---|---|---|
| The key is in `admission.active` | A root holding a slot: its tree runs, `admitted` until its architect starts the first phase | Nothing, beyond [trees waiting on a root claim](skill://legion-controller/SKILL.md#trees-waiting-on-a-root-claim) |
| The key is in `admission.waiting` and not in `admission.blocked` | A root handed to Legion in `todo`, queued in Dispatch rank order for the next free slot | Nothing: it takes the next free slot by itself |
| The key is in `admission.waiting` and in `admission.blocked` | A root in the line that waits on the Dispatch issues `admission.blocked` names: admission passes it over, and it takes no slot, until every one of them is `done` | Nothing: the wait ends when each blocker reaches `done` (a person's close, or the blocker's own merge) or its link is taken off (by a person or the tree's architect). Name the wait in the day's report |
| `phase: admitted`, in neither list, and Dispatch shows a `child_of` parent | A child: it runs or waits with its tree, the root its `child_of` chain reaches. Under a root whose `phase` is `done` it waits with it. A labelled `todo` issue here is not in the line either: one whose recorded parent is in a live tree runs in that tree (the daemon adopted it there), or is held with that parent while the parent itself is held under an unreleased child, and one whose parent is not recorded but sits under a live tree is held under an unreleased child of that tree until its architect releases it or its parent. That hold is not `admission.blocked`, which names only blocked-by links | Leave it to its tree. It runs again when its root does, or as a root of its own once it is `todo` with the `legion` label and no live tree is above it |
| `phase: admitted`, `status: todo`, in neither list, no parent | A root a person took out of the line by taking its label off | Nothing: the label puts it back in the line, and a person's label is their decision |
| `phase: done` while `status` is `in_progress`, `testing`, `needs_review` or `retro` | Someone other than Legion moved it to a running tree's status while Legion was not running it. A root that waited for a slot is taken out of the workflow at that move, since no slot would ever reach it, and the daemon sends the `unslotted` wake; a parked child stays parked. Legion starts nothing for either | Re-admit nothing yourself: `dispatch read` shows who works it now. Name it in your summary; `todo` with the `legion` label queues it again |

A root in the workflow with no slot that is in neither `admission.waiting`, nor the label-removed
row above, nor `todo` under a live tree does not stay that way: every pass of admission (a Dispatch
event, the daemon's boot read, its tick) takes it out of the workflow and sends the `unslotted`
wake. The wake is live only, so a controller that starts later finds such a root by the last row,
as its start procedure does.
