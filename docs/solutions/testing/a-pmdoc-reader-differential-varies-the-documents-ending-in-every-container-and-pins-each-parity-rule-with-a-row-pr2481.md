---
title: "A pmdoc reader differential varies the document's ending in every container and pins each parity rule with a row"
category: testing
tags:
  - pmdoc
  - differential
  - corpus
  - negative-control
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/pmdoc/gen
---

# A pmdoc reader differential varies the document's ending in every container and pins each parity rule with a row

Extends docs/solutions/integration-patterns/replacing-a-micromark-construct-fuzzes-old-against-new-with-every-construct-its-lines-meet-pr2403.md
for the Go reader's corpus generators under `pmdoc/gen/`.

- Before the first push, make the document's ending a dimension of the generator:
  - a final line feed or none;
  - trailing whitespace-only lines, with spaces, tabs and tabs a container takes part of;
  - a closing fence or none.

  Cross that dimension with every container kind, typed blocks indented inside quotes and items
  among them. Count how often each ending survives deduplication, and say the count in the
  generator's header.
- Run the reader change's differential twice. Run the corpus aimed at the change against current
  main. Run the whole corpus against the branch's own base reader. Current main can carry reader
  changes the branch has not merged (a link change, sjawhar/legion#2373, here). Then a
  whole-corpus run against main reports those as regressions of yours.
- Commit a Go row for every parity rule that only the differential proves. Delete the rule
  locally and see the suite go red. If the suite stays green, the rule is unprotected: write it
  under `Not proven / risk` in the PR body.

## Evidence

- `gen/indented-code-corpus.ts` ended every document with a line feed until round 4 (1f57b708).
  Tester rounds 1 and 2 of sjawhar/legion#2481 found end-of-document defects only with corpora of
  their own (`noeol.ts`, 6,818 documents). Review round 1 found an indented typed block at the end
  of a document that no corpus crossed.
- Against main 8a911ecc, the whole corpus showed thousands of regressions in the url, reference
  and delimiter corpora, from #2373's links. The same head against the branch base a09a2f25
  showed 0 on every GATE line.
- In round 2 of the quality review, removing `asHeldBefore`, the `keptLast` delete or
  `fenceEndsEmptyAttr` left the pmdoc suite green. Only the differentials caught it, and the PR
  body records that under `Not proven / risk`.
