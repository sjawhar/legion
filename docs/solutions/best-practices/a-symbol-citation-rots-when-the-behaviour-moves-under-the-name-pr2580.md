---
title: "A symbol citation rots when the behaviour moves under the name, so read the symbol at the head you commit"
category: best-practices
tags:
  - citations
  - documentation
  - forward-merge
  - learning-pages
date: 2026-10-31
status: active
module: docs/solutions
related_issues:
  - "INFRA-841"
  - "sjawhar/legion#2580"
  - "sjawhar/legion#2598"
---

# A symbol citation rots when the behaviour moves under the name, so read the symbol at the head you commit

Extends docs/solutions/best-practices/an-instrument-that-answers-a-smaller-question-returns-the-comfortable-answer-INFRA-141.md.

That page says to cite the calling symbol, "which cannot drift". A symbol's name cannot drift; what
it does can, and then the citation is worse than a stale line number, because the reader resolves
it, reads the opposite of your sentence, and concludes your page is wrong.

- **Read the symbol's body or doc comment at the head you commit, not at the head you learned it
  on.** A merge can keep the name and invert the behaviour. This is the one case
  `docs/solutions/legion/a-forward-merge-after-retro-falsifies-the-first-retros-own-citations-pr2542.md`
  does not cover: its "repoint it at the symbol" produces a citation that still contradicts its
  sentence.
- **When the head contradicts a sentence you still want to keep, make the citation historical in
  three parts:** the symbol, the text you read quoted, and the commit you read it at. Then say
  what the head does instead, in the same paragraph, so the reader who checks the symbol is not
  left with a contradiction.
- **Prove it is history and not rot with a pair:** the quoted text is present at the pinned commit
  and absent at the head. One `grep -F` each. A quote that still resolves at the head is an
  ordinary stale citation wearing a pin.
- **Prefer a literal nothing can move:** a constant's declaration, a function's signature, a test's
  title, a workflow step's name, or a sentence quoted from the prose you mean. Each is one
  `grep -F` away for the next reader, and each survives an edit above it.

## Evidence

sjawhar/legion#2580, INFRA-841. A learning page written by that pull request's own retro said the
claim-list check "stops at the first line that is neither a bullet nor an indented continuation
once it holds one claim", and cited the parser by file and line in
`packages/envoy/internal/broker/oauth/claims_doc_test.go`. A forward merge then brought
sjawhar/legion#2598, which widened that parser so a blank line ends the list only when the next
non-blank line is neither a bullet nor an indented continuation — the opposite rule. Review
5723093389 blocked the citation at the READY head.

Repointing at the symbol would not have helped. `documentedClaims` is still there under its own
name, and it now reads the list through `claimListNames`, whose behaviour is the new one. The page
now names the loop in `documentedClaims` **as it stood at `c10800814dec`**, quotes the comment it
had there, and says what `claimListNames` does at the head; the pair holds, with the quoted comment
present at that commit and absent at the head.

The same merge rotted a citation nobody was watching. `.github/workflows/ci.yaml`, beside its
`docs/kubernetes.md` entry, carries a comment naming the fence's read in that test file by line;
at the head that line is a comment about a silent absence, and the `os.ReadFile` it means is the
next one. A required workflow carried a wrong citation and no lane noticed, which is why this is a
class and not one page's accident.

Two citations in the same round were ordinary drift and are the contrast: `const MAX_LINE = 160`
and `wrapLongLine` in `scripts/semantic-line-breaks.ts` had both moved down the file, and naming
the symbols fixed them outright, because neither behaviour had changed.
