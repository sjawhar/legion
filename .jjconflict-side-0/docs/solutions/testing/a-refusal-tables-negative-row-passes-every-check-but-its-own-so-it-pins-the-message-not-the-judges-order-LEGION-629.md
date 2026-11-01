---
title: "A refusal table's negative row passes every check but its own, so it pins the message and not the judge's order"
category: testing
tags:
  - table-driven-test
  - negative-control
  - probe-image
  - ok-line
  - judge
  - forward-merge
date: 2026-10-09
status: active
module: packages/daemon/internal/runtime/sandbox
applies_when:
  - Adding a requirement to a judge that refuses on the first failing check in a fixed order (sandbox.judge over the OK line, the pod lane over the load answer)
  - Writing or merging a table row meant to prove one refusal by its message
  - Two branches each add a mark to legion probe-image's OK line and one forward-merges the other
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
  - "sjawhar/legion#1846"
---

# A refusal table's negative row passes every check but its own, so it pins the message and not the judge's order

Extends docs/solutions/daemon/a-new-ok-line-mark-anchors-to-the-lines-end-not-to-its-neighbours-and-the-consumer-requires-it-by-name-LEGION-578.md.

- `sandbox.judge` refuses an image on the first check that fails, in a fixed order: the contract,
  the agent-models mark, the extensions mark, the capability mark, the NATS user. A table row that
  wants to prove refusal N by its message must pass every other check: its OK line carries every
  mark but the one under test. A row that carries only the marks that existed when it was written
  proves "the first mark missing in the judge's order", and the next tree's mark — merged into the
  judge ahead of N — changes what the row's message is without anyone editing the row.
- Write such a row from the real composer with one mark withheld where the composer allows it
  (`bootprobe.OKLine` for the line every probe passed), or as the full line by hand with the one
  mark removed; name the check it proves in the row's name ("a CLI that predates the capability
  check") and keep the judge's order readable in one place (`probe.go`'s refusal sequence) so the
  row's author can see which other checks run first.
- Merging two branches that each added a mark and a "predates" row is the case where this bites:
  each side's row lacks the other's mark, both compile, each passes in its own tree, and the merged
  judge refuses one of them for the other's reason. The conflict's test-side half is to give each
  predating row the other branch's mark in the merge commit; a per-package run after the merge is
  the only thing that shows it, since no conflict marker does.
- The same shape sits in `legion probe-image`'s own pod lane, which refuses a doubly-loaded plugin
  (`pi-envoy loaded 2 times`) before it runs the capability check: a negative control for the
  capability check must leave the profile unlinked, or it proves the loads-twice refusal instead.

## Evidence

sjawhar/legion#1848 round 5 forward-merged `main` f1dcadfe (#1846). #1846 had added the
`capabilities=checked` mark with a judge check and a row `"a CLI that predates the capability
check"` whose line was `… session-storage=probed agent-models=resolved daemon-api-version=3`; the
branch had added `extensions=discovered` with its own check and row `"a CLI that predates the
discovery-on pod lane"` whose line was the same. The merged judge runs the extensions check before
the capability check (`internal/runtime/sandbox/probe.go:505-522`), so at the merge head #1846's
row failed with the branch's message — `probe_test.go:343: err = … Succeeded without the
extensions=discovered mark: its legion CLI predates the discovery-on pod lane …` for
`TestProbeImageRefusesWhatTheProbePodAnswered/a_CLI_that_predates_the_capability_check`. The
merge commit 24f2532c gave each row every other mark (`probe_test.go:315` carries
`extensions=discovered`; `:329` carries `capabilities=checked model-fallback=on`); the package
passed (`ok … internal/runtime/sandbox 40.8s`), and CI's `Tests` at 7d98d70e (run 37865275948)
agreed. The tester's round-6 proof of acceptance 4 met the pod lane's version of the order: with the
two plugins linked back into the image's profile, `legion probe-image` at 1099ab99 answered
`pi-envoy loaded 2 times …` and never reached the capability rows, so that control was run with
the profile unlinked (`test.json` `proof[0].negativeControl`).
