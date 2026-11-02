---
title: "A prose-only fix to agent-facing text is a behaviour change, and the round's own criterion is re-run at the head that ships it"
category: legion
tags:
  - review-rounds
  - prose-fix
  - role-prompts
  - behavioural-proof
  - merge-gate
date: 2026-10-31
status: active
module: packages/daemon/internal/prompts
related_issues:
  - "INFRA-336"
  - "sjawhar/legion#2585"
---

# A prose-only fix to agent-facing text is a behaviour change, and the round's own criterion is re-run at the head that ships it

A reviewer fixes a prose-only finding itself, in its own commit, and sends back only a finding that
touches code or behaviour (`packages/daemon/internal/prompts/roles/reviewer.md`,
`skill://legion-worker/references/merge-gate.md`). That cut is drawn on the diff. It mis-sorts the
most consequential prose in a repository whose product *is* agent-facing text.

- **Judge "prose-only" by what the file is for, not by whether the diff touches code.** A sentence
  in `packages/daemon/internal/prompts/`, in `skills/`, or in any other text an agent is given as
  its instructions, is the behaviour. Changing it changes what agents do.

- **A change to that text ships unproven unless the round's own behavioural criterion runs at the
  head that carries it.** Static lanes — the Go tests over the embedded parts, a lint, a grep that
  the sentence is present — check that the words are there, not what an agent does with them.

- **So a reviewer with such a fix either runs the criterion itself before approving, or sends the
  finding back.** It already waits for that head's checks before it approves; running the round's
  own criterion is the same wait with the evidence the approval needs.

- **A later role that meets the gap closes it by measuring, not by flagging it.** Retro may change
  only `docs/solutions/` and remove the handoff directory, but running a session writes nothing in
  the workspace, so the proof is still available to it.

## Evidence

In INFRA-336 the same sentence was rewritten twice by reviewer prose commits, both after a
tester's verified head.

The first, `dc17476b`, is why the tester's next round was a **full** round rather than the bare
gates a forward merge allows: its handoff records that between the head it last verified and the
merge tip sat that commit, "which rewrote the handoff sentence in `go/worker-common.md` and in all
four role residues and in `skills/legion-worker/SKILL.md` — product text of this very change that
no tester had read".

The second, `59a4e931`, rewrote the same sentence again, after the tester's last verified head
`4389e477`. The reviewer's own checks that round were static throughout: `go test
./internal/prompts/...`, biome, the harness's unit test, the acceptance greps, a YAML parse. The
change's behavioural criterion — that an agent reading the composed prompt writes a handoff with
no `completed` key and with `previousHandoff` — had no evidence at the wording that ships.

Retro ran it: three headless sessions on the tester, reviewer and implementer prompts composed
from the shipping head's daemon module all wrote handoffs with no `completed` key and with
`previousHandoff`, and the reviewer session's also carried `headReviewed`. The gap closed, and it
closed because someone ran the sessions, not because someone noticed. Both `completed` and
`previousHandoff` are gone since sjawhar/legion#2354, which made each handoff a versioned record
on its Dispatch issue; what this page is about is re-running the round's own criterion at the head
that ships, whatever the wording asks for.
