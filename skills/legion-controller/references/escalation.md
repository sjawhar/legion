# Architect escalation

Read this when a tree's architect sends you a matter, when a `held` wake carries
`reason: "escalated"`, or when an `update-stalled` wake says the daemon cannot replace an issue's
pod. Three matters come to you from an architect: a child that looks independent of its tree,
capacity (an architect says its tree cannot run what it needs under the slots it has), and a
conflict between trees. An issue-design decision is not one of them: the owning architect writes
it as a decision block and opens its own finding asks. Send a design decision back to that
architect with `envoy_publish` to its role, because it is not yours to make.

For capacity, the only lever is order. You cannot change `admission.cap`: it is the operator's
`legion.yaml` setting, and `set_status` is your only admission move. Read `admission.active` and
`admission.waiting`, tell the architect its root's place in the line with `envoy_publish`, and
open a finding ask when the deployment needs a larger cap.

For a conflict between trees, read both roots in `read_state` and Dispatch, publish what you read
to both architects' roles, and open a finding ask when the two need a person's decision.

## A child that looks independent

Check the child and its parent against `read_state` and Dispatch. When it still looks independent,
open an ask on your report issue, as the skill's "Findings, and the issues you file" says. The ask
names the child, why it looks independent and the root you would file, with three options:
`File an issue` (a root a person takes), `File it for Legion` (a root Legion runs) and
`Do not file` (it stays in its tree). Never add the `legion` label to a child or cut its
`child_of` link, because that admits it as a root while its tree still records it. Only on a
person's yes:

1. File the root with no `--parent`:
   `dispatch issue --project <PROJECT> --title '<title>' --spec-file -`. Its spec cites the ask and
   carries its `Filed for` line (`skill://legion-controller/references/finding-asks.md`). Add
   `--label legion` only when the answer asks Legion to run it. `<PROJECT>` is the issue key's
   prefix before `-<n>` (`ACME-3` → `ACME`), which `read_state` shows as `daemon.project`; it is
   not the lowercase project token in role names such as `legion-<project>-controller`.
2. Park the child (`set_status` to `icebox`) and comment a pointer to the new root on it. You
   cannot close it, because only its architect or the daemon sets `done`.
3. A root filed with the label opens in `triage`, and its triage wake brings it back to you
   (`skill://legion-controller/references/triage.md`).

## An update round that is not finishing (`update-stalled`)

`{kind: "update-stalled", reason, update: {round, pod?, reasons, waiting}}`, on the issue whose
pod it is. The daemon must replace the pod that runs an issue's roles, because the configuration
the pod was built from has moved. It never replaces a live pod where it stands: every role of the
issue works on that pod's disk, so the daemon opens an update round instead and tasks each worker
in the pod to push its work and answer `ready_for_update` first. This wake says that round is not
finishing.

It names the round (`round`: `pod:<pod uid>` for the whole pod, `plugin:<daemon contract>` for
one claim's own process), the pod it was opened over (`pod`, absent for a claim's own plugin
round), the drift the task states (`reasons`), the one line that says why it is stuck (`reason`),
and every claim the round is blocked on (`waiting`, each with its `claim` token, `role`, `state`
and `answered`). Nothing marks a worker whose plugin is too old to call `ready_for_update`: such
a worker is asked like any other and answers by ending the turn the daemon's task started, so a
round over one finishes by itself and no drain is asked of anybody.
One round wakes you once, however many sweeps find it open, so handle it when it arrives.
Read `read_state` and the issue first, as for every wake: a round that finished since needs
nothing.

Take each `waiting` entry by its shape:

| Entry | What it is | What you do |
|---|---|---|
| `answered: true` | It has already answered this round. The round is blocked on the daemon's own side: a turn that has not ended, or a pod replacement the cluster keeps refusing. `reason` names which. | Send it nothing. A message to it starts a turn, and a turn after its answer takes that answer back, which puts the round further from finishing. Open a finding ask naming the issue, the round and `reason`, with the turn or the `ReplacePod` error it names, so an operator reads the pod. |
| `state: "failed"` | Its launches or prompts ran out. Nobody can say whether its workspace holds unpushed work, so the round waits for it. | It answers once it runs again. When the issue's `phase` is `held`, `envoy_publish` to the tree's architect role and ask it to `retry_or_escalate` that role. Otherwise the retry is the operator's (`legion claims retry`), so open a finding ask. |
| `state: "suspended"` | An operator's own suspension holds it: the daemon revives a suspension of its own for the task. The claim already holds the task and takes it on the resume. | Open a finding ask naming the issue and the role, so the operator resumes it. |
| any other state | Its worker holds the task and has not answered. The daemon asks it again every `pod_update_retask_hours`. | `envoy_publish` to `notifications.role.<claim>` with the issue and the update task's three steps (push every commit to keep to the issue branch, call the `legion` tool with `op: "ready_for_update"` and the `round` this notice names, which is the id the worker's own task carries, end the turn), and say in the same message that a worker whose `legion` tool has no `ready_for_update` operation does step 1 and step 3 alone. Name it in your summary. Your publish starts a turn of its own, which answers nothing: only the end of the turn the daemon's own task started is that worker's answer, so what your message buys is the next ask. |

Park nothing and write no status for this wake. The issue's workflow runs on while the round
waits, and replacing the pod is the daemon's own move once every worker has answered.
