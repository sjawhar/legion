---
title: "A selection rule written for authors is applied by a script to every case before review"
category: best-practices
tags:
  - documentation
  - selection-rule
  - test-tags
  - review
date: 2026-10-27
status: active
module: packages/dispatch
related_issues:
  - "INFRA-521"
  - "sjawhar/legion#2313"
---

# A selection rule written for authors is applied by a script to every case before review

Extends docs/solutions/best-practices/a-number-or-a-guarantee-restated-in-prose-is-a-copy-that-drifts-cite-or-generate-it-INFRA-236.md.

When a document tells future authors which tests, files or rows belong to a selection, and the
selection itself was made another way (a table, a reading, a diff), check the document before you
send it to review:

- Write a throwaway script that applies the document's text literally to every case: every
  condition and every exception, nothing the text leaves out. Compare its verdicts with the real
  selection, and fix the text until the script reports 0 mismatches.
- Then move one item in a scratch copy and confirm the script reports it.
- Tie every threshold or term to a code symbol (`under 768 px, PHONE_VIEWPORT_QUERY`), never to a
  word a reader has to interpret (`desktop width`).
- Keep the rule in one place. Other files point to it instead of restating it.
- If only prose guards the selection afterwards, say so in the pull request's risks.

## Evidence

sjawhar/legion#2313: the tagging rule in `packages/dispatch/AGENTS.md` ("Phone acceptance") went
through three more rounds after the first review. Read literally, the round-2 text (6d04381c,
2cd0e103) untagged `issue.e2e.ts:1226`, an `iphone`-only test, which would then run nowhere. The
round-3 text (0fc4e40a) left "desktop width" undefined, so `keyboard-palette.e2e.ts:1039` at 1024 px
was ambiguous. Copies of the rule in `README.md` and the Playwright config comment lacked its
exceptions. The text settled (96fcf1de) only once the tester's script applied it to all 663 test
calls with 0 mismatches, and flagged both tests in a copy where one tag was moved.
