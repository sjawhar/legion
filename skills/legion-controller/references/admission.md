# Reading admission

`read_state` says where each issue Legion records stands, and its `phase` alone does not: an issue
is `admitted` from the moment the daemon records it until its first phase starts, whether it runs,
waits, or neither. Read its key against `admission` and its tree's root.

| What `read_state` shows | What it means | What you do |
|---|---|---|
| The key is in `admission.active` | A root holding a slot: its tree runs, `admitted` until its architect starts the first phase | Nothing, beyond [trees waiting on a root claim](skill://legion-controller/SKILL.md#trees-waiting-on-a-root-claim) |
| The key is in `admission.waiting` | A root handed to Legion in `todo`, queued for the next free slot: a root a person answered while it waited in `awaiting_person` comes first, by the time it was answered, and then every root that never ran, in Dispatch rank order | Nothing: it takes the next free slot by itself |
| `phase: awaiting_person`, no slot, `waitingOn` | The root already ran but now waits only on a person. Its architect session is suspended after its turn, its tree remains live, and its slot is available for other work. `answeredAt` means it is before never-run roots in `admission.waiting` | Do nothing: do not park or re-admit it, and label or admit nothing for the slot it returned ([Free slots](skill://legion-controller/SKILL.md#free-slots)). It resumes itself with its same architect session when a slot is free. If the label disappears or Dispatch shows a foreign claim, the daemon sends `unslotted` and does not resume it |
| `phase: admitted`, in neither list, and Dispatch shows a `child_of` parent | A child: it runs or waits with its tree, the root its `child_of` chain reaches. Under a root whose `phase` is `done` it waits with it | Leave it to its tree. It runs again when its root does, or as a root of its own once it is `todo` with the `legion` label |
| `phase: admitted`, `status: todo`, in neither list, no parent | A root a person took out of the line by taking its label off | Nothing: the label puts it back in the line, and a person's label is their decision |
| `phase: done` while `status` is `in_progress`, `testing`, `needs_review` or `retro` | Legion is not running it under that status, and it sent the `unslotted` wake. Either someone other than Legion moved a root that waited for a slot there, so the daemon took it out of the workflow at that move since no slot would ever reach it; or a root waiting on a person could not be resumed — its `legion` label is gone, or a person or an outside session took its Dispatch claim — and the daemon took it out the same way, leaving the `in_progress` it ran with. A parked child stays parked. Legion starts nothing for any of them | Re-admit nothing yourself: `dispatch read` shows who works it now. Name it in your summary; `todo` with the `legion` label queues it again |

A root in the workflow with no slot that is in neither `admission.waiting` nor the label-removed
row above does not stay that way: every pass of admission (a Dispatch event, the daemon's boot
read, its tick) takes it out of the workflow and sends the `unslotted` wake — an unanswered
`awaiting_person` root only when its label or its Dispatch claim makes a resume unsafe. The wake
is live only, so a controller that starts later finds such a root by the last row, as its start
procedure does.
