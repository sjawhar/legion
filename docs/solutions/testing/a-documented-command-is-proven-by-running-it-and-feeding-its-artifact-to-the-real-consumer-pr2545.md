---
title: "Prove a documented command by running it and feeding its artifact to the consumer, with the retired form as the negative control"
category: testing
tags:
  - proof
  - negative-control
  - instructions
  - changelog
date: 2026-10-30
status: active
module: docs
related_issues:
  - "sjawhar/legion#2545"
---

# Prove a documented command by running it and feeding its artifact to the consumer, with the retired form as the negative control

Extends docs/solutions/testing/instruction-text-is-proven-on-the-staged-partition-and-the-daemons-prompt-snapshot-INFRA-227.md.
That document says where instruction text is proven to have arrived. This one says how to prove the
text is true when the instruction is a command and the name it produces.

- Run the command the text documents, from the directory the text names, and keep its artifact.
  A grep of the tool's configuration beside a grep of your new sentence proves only that two
  strings agree; it does not produce the name a reader will get.
- Give that artifact to the tool that consumes it, by the command the workflow runs, and read what
  that tool did with it.
- Make the negative control the form the text no longer teaches, driven through the same consumer.
  The old text put back is the control for prose; for a command it is the retired artifact.
- Say which part of the name the prose cannot predict. A name a template builds from a timestamp
  or a kind is knowable only from a run.
- A wrong instruction with no failure mode is the case this exists for: nothing is red, so no check
  will tell you the sentence is wrong.

## Evidence

sjawhar/legion#2545 corrected instructions that named a changelog fragment
`<ISSUE KEY>-<slug>.yaml` and ran `changie new --custom Issue=<KEY>`, while every
`packages/*/.changie.yaml` sets, at line 32,
`fragmentFileFormat: '{{.Custom.Issue}}-{{.Kind}}-{{.Time.Format "20060102-150405"}}'`.

- The documented command wrote this pull request's own fragment,
  `packages/pi-legion/.changes/unreleased/pr2545-Fixed-20261030-055126.yaml`. The `<Kind>` and
  `<YYYYMMDD-HHMMSS>` segments come from the run; no reading of the prose gives them.
- The consumer is the fold: `.github/scripts/release-push.sh fold-plan` and `fold-notes`, the two
  commands `.github/workflows/changelog-fold.yaml` runs at lines 97 and 111. Driven on a clone at
  the head, `fold-plan` listed a second run's `pr2545-Added-20261030-055929.yaml` and `fold-notes`
  rendered its entry.
- Negative control: the branch-name form the old text taught,
  `packages/contracts/.changes/unreleased/sjawhar/fix-thing.yaml`, committed into the same clone →
  `fold-plan` exit 1, refused by name (`.github/scripts/release-push.sh:832` with its message at
  `:857`). Removing that path restored exit 0.
- Why the drift was invisible: `fold-plan` accepts any regular `*.yaml` file in the directory, so
  a fragment named by a Dispatch key folded perfectly. The instruction was wrong without being
  broken, and no job compares an instruction with the configuration it describes — `AGENTS.md`'s
  only CI reader is `packages/pi-legion/src/skills-guard.test.ts`, a sweep for chosen phrases, and
  no filter in `.github/workflows/ci.yaml` names `docs/solutions/**` at all.
