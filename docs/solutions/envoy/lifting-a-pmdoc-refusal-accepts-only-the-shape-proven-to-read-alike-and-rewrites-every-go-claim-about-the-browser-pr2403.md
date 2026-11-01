---
title: "Lifting a pmdoc refusal accepts only the shape proven to read alike, and rewrites every Go claim about the browser"
category: envoy
tags:
  - dispatch
  - pmdoc
  - refusals
  - differential
  - browser-parity
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
---

# Lifting a pmdoc refusal accepts only the shape proven to read alike, and rewrites every Go claim about the browser

Extends docs/solutions/envoy/a-writer-that-checks-its-output-by-reading-it-back-keeps-stored-bytes-only-when-the-new-reader-joins-every-step-of-the-old-ladder-INFRA-534.md.

- Removing or narrowing a refusal in `pmdoc/refusals.go` changes what a document write stores. A
  refusal guards a whole family of shapes where Go and the browser editor disagree, but its reason
  names only the example its author saw.
- Before the push, build a corpus that crosses the refused shape with every container around it
  (list item, quote, typed block) and every kind of block inside it (paragraphs, a list, a quote,
  code, an empty one), with blank lines and an unclosed fence. Run
  `pmdoc/gen/differential.sh --base main --corpus <that.jsonl>`. Its misread gate (main refuses,
  head stores something that reads back otherwise) must be zero.
- Accept only the narrowest shape both parsers are shown to read alike, and refuse everything else.
  Never delete the refusal and then add a refusal for each new shape a probe finds.
- A census of stored versions shows only that no backfill is needed. It says nothing about the
  misreads of new writes.
- pmdoc's refusal reasons and comments describe the browser editor's parser. When a proof-editor
  change alters a reading (a fuzzer's residual included), grep `pmdoc` and `packages/envoy/AGENTS.md`
  for `browser editor`, and check the Go helpers that list the browser's containers
  (`render.go` `holdsAContainerTheBrowserDrops`, `lines.go` `frontmatterAttempt`). Rewrite each
  claim the change makes false, then apply the rule above to the refusal behind it.

## Evidence

- Pull request #2403 made the browser read `:::` blocks as containers, so a footnote definition
  inside one now reaches a call before the block. `refusals.go`'s reason "which the browser editor
  refers to only from inside a typed block or after it" became false (the tester's red test,
  b5d904f9). The refusal was then removed outright, on two example documents (ca82ad63).
- `differential.sh --base main` passed over the 100 in-repo documents at 799b379f. The reviewers'
  137,187 generated probes then found 12 misreads that main had refused. A refusal for a definition
  with a list item around it (c54bf052) left a list inside the definition misread, which the next
  test round found (46396a19).
- Only an allowlist closed the family: a definition in a typed block is stored when it holds
  paragraphs alone and no list item stands around it (fef578d6, `refusals.go:85-100`). The stored
  census found 0 of 31,160 versions holding the shape at every round, so it caught none of this.
- The same change had to add typed blocks to `holdsAContainerTheBrowserDrops` and wrap the typed
  block parser in `frontmatterAttempt` (`parse.go:42`).
