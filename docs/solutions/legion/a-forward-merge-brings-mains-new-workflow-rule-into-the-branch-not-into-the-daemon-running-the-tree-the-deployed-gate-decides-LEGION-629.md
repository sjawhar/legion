---
title: "A forward merge brings main's new workflow rule into the branch, not into the daemon running the tree: the deployed gate decides"
category: legion
tags:
  - forward-merge
  - workflow-rules
  - daemon-release
  - installed-plugin
  - retro
  - ready-gate
date: 2026-10-09
status: active
module: packages/daemon/internal/prompts
applies_when:
  - A forward merge of main brings a change to Legion's own workflow (what retro commits, what READY refuses, what the merger's tip check allows) into your branch's AGENTS.md, skills/ or prompt sources
  - A handoff or a role's plan cites the branch's AGENTS.md or skills/ for a Legion workflow rule
  - You must decide what a gate the daemon enforces will do before you commit or push
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
  - "LEGION-605"
  - "sjawhar/legion#1857"
---

# A forward merge brings main's new workflow rule into the branch, not into the daemon running the tree: the deployed gate decides

- A workflow rule that lands on `main` reaches your branch's text (AGENTS.md, `skills/`,
  `packages/daemon/internal/prompts/`) with the next forward merge and reaches the gate that will
  run only when the operator deploys that release. Until then the daemon running your tree enforces
  its own release: the prompts it embeds, composed into each role's system prompt at launch, and
  the plugin installed in the pod, whose `dist/skills/` is what `skill://` serves. Before acting on a
  rule the branch states, read the same rule in your system prompt and in the installed skill
  (`/opt/legion/pi-legion/dist/skills/…`, version in `/opt/legion/pi-legion/package.json`). When they
  disagree with the branch, follow the deployed text and tell the architect both texts and which
  role will meet the gap.
- Nothing in `legion state` names the daemon's release, and `legion version` names the pod's CLI
  (its image), not the daemon. The one direct read of the daemon's release is a sentence of your
  system prompt that the rule change rewrote: `jj diff -r <the rule's commit> --git
  packages/daemon/internal/prompts/go/<role>.md` shows the old and new sentence; whichever your
  prompt carries is the daemon you have.
- Two releases' gates can be mutually exclusive, and the live one decides. Under the pre-#1857
  daemon the merger's tip check (`jj diff --from <approved> --to <tip> --summary` with
  `'~docs/solutions'` appended must print nothing) treats any other change above the approved head,
  a `.legion/<issue>/` removal included, as voiding the approval, and the merger sends the head back
  to review; under #1857's daemon `handoff complete --ready` refuses a head that still carries
  `.legion/<issue>/`, the merger tells the architect, and the architect moves the issue back to
  retro (`request_backward_move`) for the removal. A retro that follows the deployed text under the
  old daemon costs at worst that designed backward move if the daemon is upgraded before READY; a
  retro that follows the branch's newer text under the old daemon produces a head the deployed
  merger reports as voiding the approval.
- A handoff that cites the branch's AGENTS.md for a workflow rule the daemon enforces is the tell.
  Cite the daemon's text (the system prompt's sentence, the installed skill's path and line) for a
  rule about what the daemon will do; cite the branch's text only for what the code under review
  does.

## Evidence

sjawhar/legion#1848 (LEGION-629), 2026-10-09. #1857 (LEGION-605, "retro removes the issue's
`.legion/` before READY") merged to `main` at 05:24Z and shipped in `legion` v10.2.1 at 05:28Z;
the forward merge 66c26bbc brought it into this branch (its one conflict was that rule's rewrite of
two AGENTS.md command lines beside this branch's edit of the next line). The daemon running the tree
restarted at 06:13Z (boot 21) on the older release: the implementer's system prompt carried the
pre-#1857 sentence `No role pushes a .legion/ deletion: the reviewer approves a head that still
carries .legion/, your retro commits go above that approved head, and the merger's READY names a
head that still carries .legion/` — the sentence `jj diff -r 7407b90a2d5e --git
packages/daemon/internal/prompts/go/implementer.md` shows #1857 replacing — and the pod's installed
pi-legion was 8.3.1, whose `legion-retro/SKILL.md` says retro writes no `.legion` file and whose
`merge-gate.md` (lines 79-98) has the merger's `'~docs/solutions'` check. The tester's handoff
(`observations[1]`) and the reviewer's (`notProven[2]`) both wrote "the retro after this approval is
where the removal lands", citing the branch. The third retro left `.legion/LEGION-629/` in place,
wrote this note, and told the architect both texts and the two outcomes; the fresh-eyes reader of
that retro reached the same reading independently from `/opt/legion/pi-legion/package.json` and the
installed skills.
