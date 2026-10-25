# Architect escalation

Three matters come to you from a tree's architect: a child that looks independent of its tree,
capacity, and a cross-tree conflict. The owning architect writes an issue-design decision as a
decision block and opens its own finding asks, not the controller.

For a child that looks independent, verify the child and its parent against current daemon state
and the Dispatch issue. When it still looks independent, ask a person whether to re-file it, as
[Findings, and the issues you file](skill://legion-controller/SKILL.md#findings-and-the-issues-you-file) says: the ask names the
child, why it looks independent and the root you would file, with the three options (`File an
issue` re-files it as a root a person takes, `File it for Legion` as a root Legion runs, `Do not
file` keeps it in its tree). Only on the person's yes:

1. File the root with `dispatch issue --project <KEY> --title '<title>' --spec-file -` (no
   `--parent`), its spec citing the ask and carrying its `Filed for` line ([Findings, and the issues
   you file](skill://legion-controller/SKILL.md#findings-and-the-issues-you-file)); add `--label legion` only when the answer asks
   Legion to run it. `--project` is the issue key's prefix before `-<n>` (e.g. `LEGSMOKE-3` → `LEGSMOKE`):
   the project key exactly as `legion.yaml` writes it, which `read_state` shows as
   `daemon.project`. It is not the lowercase project token in role names such as
   `legion-<project>-controller`.
2. Park the child (`set_status` to `icebox`) and leave a pointer to the new root issue. The
   controller's capability is `todo`/`backlog`/`icebox` only — only the owning architect or the
   daemon closes an issue as `done`.
3. A root filed with the label opens in `triage` and is triaged as any labelled root
   ([New issue triage](skill://legion-controller/SKILL.md#new-issue-triage)).

Never promote a child in place. Resolve capacity and cross-tree conflicts from verified
state, routing design decisions back to the owning architect when they are not controller
judgments.
