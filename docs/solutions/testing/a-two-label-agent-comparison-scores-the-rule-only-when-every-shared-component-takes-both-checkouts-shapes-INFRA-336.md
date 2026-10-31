---
title: "A two-label agent comparison scores the rule only when every shared component takes both checkouts' shapes"
category: testing
tags:
  - skill-scenarios
  - two-label-comparison
  - scorer
  - schema
  - confound
  - prompt-changes
date: 2026-10-31
status: active
module: packages/pi-legion/scripts/skill-scenarios
related_issues:
  - "INFRA-336"
  - "sjawhar/legion#2585"
---

# A two-label agent comparison scores the rule only when every shared component takes both checkouts' shapes

`rig.sh profile <label> <checkout>` for each version, then `rig.sh batch <scenario> <n> head base`,
runs each label's agent under **its own checkout's instructions** and scores every label's runs
with **one scorer**. That split is what makes the comparison fair, and it is also where it breaks.

- **Every component the labels share must accept every shape any compared checkout's instructions
  can produce.** The scorer, its schemas, its seeded fixtures and any judge are shared; the
  instructions are not. A shared component that refuses one label's shape marks that label down
  for the text under comparison rather than for the rule under measurement.

- **The failure is silent and directional.** The harness still prints numbers, and the numbers
  favour whichever label the shared component was written against. Nothing errors, so only a
  reader who asks the next question finds it.

- **Before you add a constraint to shared scoring code, ask whether a run of the other label would
  satisfy it.** A field the new instructions require is exactly the constraint that fails this
  question, because the old instructions never mention it.

- **Enumerate the artifacts the labels share and decide, per artifact, whether it is input the
  rule scores or a confound.** A seeded fixture the harness writes from the batch's own checkout
  reaches both labels; it is harmless only when the rule does not score it, and that is a thing to
  check rather than assume.

- **Keep the reason at the site.** A schema kept loose on purpose reads like an oversight to the
  next quality pass. Put the invariant in the doc comment beside the field, or it will be
  tightened again.

## Evidence

INFRA-336 added `previousHandoff` to the handoff a Legion phase worker writes. A simplify pass in
the first implementing round made the field **required** in `TestHandoff`, the schema
`packages/pi-legion/scripts/skill-scenarios/score.ts` scores a tester's handoff with — a reasonable
tightening read on its own.

The documented comparison for that scenario is `./rig.sh batch tester-proof 5 head base`, where
`base` is a checkout of `main`, whose tester prompt asks for `completed` and never mentions
`previousHandoff`. Every base-label run therefore failed the schema, which `testerProof` records
as `proofProblems=["not a test handoff: … at previousHandoff"]` and scores `pass=false`. The
comparison would have measured the prompt it varies rather than the tester-proof rule it exists to
score.

The tester caught it with a red test — a handoff body in the old shape, which the schema must
read — and the fix was `previousHandoff: z.string().optional()` with the reason written into the
schema's doc comment: 67 pass / 1 fail before, 68 pass / 0 fail after. The comment then did its
job in the same pull request: a code-quality reviewer raised the loose field again, and the
reviewer closed the finding on the comment.
