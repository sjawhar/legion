---
title: "A write through a lossy projection keeps the node it did not change and refuses what the projection cannot show"
category: envoy
tags:
  - dispatch
  - refusal
  - write-validation
  - pmdoc
  - budget
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2525"
---

# A write through a lossy projection keeps the node it did not change and refuses what the projection cannot show

Extends docs/solutions/envoy/a-refusal-of-a-shape-stored-data-may-hold-refuses-only-what-the-write-adds-counted-by-content-INFRA-172.md.

A route that writes rich stored nodes from a caller's flat projection of them - an ask row's
`Label: Description` for a list item that may hold a sub-list, a second paragraph, a link and
bold - follows these rules.

- **Never rebuild the structure from what was sent.** Pair each sent value with the node already
  holding it, keep an unchanged node as it is, and rewrite only the part that changed. A rebuild
  deletes everything the projection cannot carry, and the caller never saw it to miss it.
- **Pair on the whole value first, then on its key.** Two nodes can carry one key, and a person's
  words belong to the node they wrote them under: key-first pairing moves a sub-list under another
  value's line and drops the node that said what the edit left alone, with nothing reported.
- **Round-trip the merged result through the production markdown pipeline, then put identity
  back** - the collection's own block id, the kept node itself, and the rewritten node's
  attributes and the blocks under its line - so the structure carries the attributes any other
  write gives it while the inner block ids a comment anchors into survive.
- **Charge the write's budget with what the caller sent, and parse content the document already
  holds on a budget of its own.** Only a parse counts elements: `pmdoc.WriteBudget.RefusePlainText`
  weighs letters, so a route that pre-weighs the caller's text and then parses everything on a
  fresh budget lets one write make about twice the per-write cap. Charging the write for the
  preserved content is the opposite error: it refuses an edit of one value's line for the weight
  of another value's sub-list, which the caller never sent and cannot make smaller.
- **Weigh the refusal on the write's own plan, never on a second computation of it.** Build the
  guard from the merge's result - an entry whose node the merge does not keep whole is a rewrite
  to weigh, a node no entry holds is a drop - so the guard and the writer cannot drift, and say in
  the code which refusal comes first and that the whole edit is refused on it.
- **A shared helper whose other caller cannot answer a refusal keeps the refusal outside it.**
  Settlement's restore of a decided ask's recorded wording goes through the same merge with nobody
  to answer, so the merge always drops and the route refuses before it writes. Refusing inside the
  merge instead leaves such a block disagreeing with its row for good and logs the same refusal at
  every settlement of that document.
- **A refusal quotes what the projection could not show, as data, and names a step every owner can
  run.** Quote the node's markdown with `strconv.Quote`, never into the reason raw; name the
  command with each owner form it has (`--issue <KEY>`, or `--project <KEY>` for a project's
  document); and claim no uniqueness you cannot hold - "the one case a document edit cannot reach"
  was false the moment another refusal named a second one.

## Evidence

sjawhar/legion#2525, in `packages/envoy/internal/dispatch/docs/ask_option_merge.go`,
`ask_block_text.go` and `packages/envoy/internal/dispatch/api/asks.go`. An agent's options edit of
a Dispatch decision rebuilt the whole bullet list, so a sub-list, a second paragraph, a link's
address and bold under an option were deleted unseen; the ask row shows an option's label and
description alone, so neither the sender nor its reader could see the loss.

Four review rounds found, in turn: the rebuild itself; a rewritten line losing its own markup; a
duplicate label moving a person's sub-list under the other option; the caller's new items escaping
the write's budget; and the row taking a reading no parse takes - for a block the repair leaves
holding two lists, `editAsk` wrote the joined options of both, first from the block and then, when
only that copy was guarded, from the request. The row now has two sources
(`previous.Options` under `AskBlockText.SeveralOptionLists`, `stored.Options` otherwise), and any
new field copied from the request rather than from the stored reading reopens that finding;
`TestEditOfAnAskWhoseBlockHoldsASecondListKeepsTheRowsOptions` is the only lock.
