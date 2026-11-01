# Architect escalation

Read this when a tree's architect sends you a matter, or a `held` wake carries
`reason: "escalated"`. Three matters come to you: a child that looks independent of its tree,
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
