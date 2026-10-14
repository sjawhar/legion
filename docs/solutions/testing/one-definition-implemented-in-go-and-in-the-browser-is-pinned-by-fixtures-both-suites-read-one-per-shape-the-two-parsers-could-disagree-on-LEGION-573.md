---
title: "One definition implemented in Go and in the browser is pinned by fixtures both suites read, one per shape the two parsers could disagree on"
category: testing
tags:
  - shared-fixtures
  - contracts
  - dual-implementation
  - markdown
  - code-fence
  - dispatch-web
date: 2026-10-14
status: active
module: packages/contracts/fixtures
related_issues:
  - "LEGION-573"
  - "sjawhar/legion#1944"
symptoms:
  - "the server and the browser compute the same thing with two parsers"
  - "a review round finds a markdown shape one implementation treats as structure and the other as text"
  - "the second round finds the next such shape"
---

# One definition implemented in Go and in the browser is pinned by fixtures both suites read, one per shape the two parsers could disagree on

When a server rule and a browser control must agree on one computation and share no code (the
carry's cut of a document's decision blocks: `CutDecisionBlocks` over the parsed tree in
`packages/envoy/internal/dispatch/docs/approval_carry.go`, `cutDecisionBlocks` over lines in
`packages/dispatch/web/src/features/doc/decision-blocks.ts`), the two drift on every shape one
parser sees as structure and the other as text.

## The rule

- Put the fixtures where both suites can read them from the first commit
  (`packages/contracts/fixtures/decision-blocks/`: `approved.md`, `answered.md`,
  `paragraph-changed.md`, `fenced-example.md`), and make both suites read the files, never a
  copy: `docs/approval_carry_test.go` (`decisionBlockFixture`) and the web's
  `decision-blocks.test.ts` and `AskCard.test.tsx` read the same paths.
- Enumerate the divergence shapes as fixtures up front, not as review findings: a block inside a
  code fence (backtick and tilde), a fence with an info string that itself holds the fence
  character, a block in a list item, a block in a quote, a fence nothing closes. Each is one file
  both suites assert the same way.
- The Go test also checks each fixture is written as `pmdoc.Render` writes it, so the file is the
  stored form the server compares, not a hand-written approximation.

## Tests that pin it

`TestCutDecisionBlocksKeepsADecisionBlockWrittenInsideACodeFence`,
`TestCutDecisionBlocksEqualsAcrossAnswersAndDiffersOnAParagraph` (Go); "a decision block written
as an example inside a code fence is text and stays", "a tilde fence, a fence inside a quote and a
fence nothing closes are kept whole too" (`decision-blocks.test.ts`); "the diff cuts every decision
block, so answers alone leave nothing changed" (`AskCard.test.tsx`).

## Evidence

sjawhar/legion#1944 (LEGION-573): the duplicate cut drew a round-1 finding (the browser cut a
block written as an example inside a code fence, which the server's parsed cut kept) and a
round-2 finding (a backtick in a fence's info string), each a shape only the line-based cut got
wrong. The shared fixture directory existed from the first round; the fence shapes were added to
it one round at a time.
