# Reading admission

`read_state` says where each issue Legion records stands, and its `phase` alone does not: an issue
is `admitted` from the moment the daemon records it until its first phase starts, whether it runs,
waits, or neither. Read its key against `admission` and its tree's root.

| What `read_state` shows | What it means | What you do |
|---|---|---|
| The key is in `admission.active` | A root holding a slot: its tree runs, `admitted` until its architect starts the first phase | Nothing, beyond [trees waiting on a root claim](skill://legion-controller/SKILL.md#trees-waiting-on-a-root-claim-go-daemon) |
| The key is in `admission.waiting` | A root handed to Legion in `todo`, queued in Dispatch rank order for the next free slot | Nothing: it takes a slot before anything you add |
| `phase: awaiting_person`, no slot, `waitingOn` | The root already ran but now waits only on a person. Its architect session is suspended after its turn, its tree remains live, and its slot is available for other work. `answeredAt` means it is before never-run roots in `admission.waiting` | Do nothing: do not park or re-admit it. It resumes itself with its same architect session when a slot is free. If the label disappears or Dispatch shows a foreign claim, the daemon sends `unslotted` and does not resume it |
| `phase: admitted`, in neither list, and Dispatch shows a `child_of` parent | A child: it runs or waits with its tree, the root its `child_of` chain reaches. Under a parked root (its `phase` is `done`) it is parked with it | Leave it to its tree. It runs again when its root does, or as a root of its own once it is `todo` with the `legion` label |
| `phase: admitted`, `status: todo`, in neither list, no parent | A root a person took out of the line by taking its label off | Nothing: the label puts it back in the line, and a person's label is their decision |
| `phase: done` while `status` is `in_progress`, `testing`, `needs_review` or `retro` | Someone other than Legion moved it to a running tree's status while Legion was not running it. A root that waited for a slot is taken out of the workflow at that move, since no slot would ever reach it, and the daemon sends the `unslotted` wake; a parked child stays parked. Legion starts nothing for either | Re-admit nothing yourself: `dispatch read` shows who works it now. Name it in your summary; `todo` with the `legion` label queues it again |

A root in the workflow with no slot that is neither waiting nor paused by its label does not stay
that way: every pass of admission (a Dispatch event, the daemon's boot read, its tick) takes it out
of the workflow and sends the `unslotted` wake. The wake is live only, so a controller that starts
later finds such a root by the row above, as its start procedure does.
