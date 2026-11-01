---
title: "A review fix that deletes code greps the prose the pull request added for the deleted name, and writes a library's behaviour from its source, not from the absence of your own handler"
category: legion
tags:
  - legion-worker
  - review-round
  - documentation
  - agents-md
  - prosemirror
date: 2026-10-12
status: active
module: packages/dispatch
related_issues:
  - "LEGION-695"
  - "sjawhar/legion#1928"
---

# A review fix that deletes code greps the prose the pull request added for the deleted name, and writes a library's behaviour from its source, not from the absence of your own handler

- When a review round removes a property, a branch or an override, grep the pull request's own
  prose — the `AGENTS.md` passages, doc comments, the changelog entry — for the deleted name in
  the same commit. A sentence that documents the removed code as a rule is the standing
  justification for a future worker to put the defect back, with the document as the citation.
  Here the check is `jj diff --from <base> --to @ -- '*.md' | grep <name>` plus a grep of the
  changed source files' comments.
- A sentence about what happens where your code has no handler ("the version view has no click
  handler, so the browser follows the href") is a claim about the libraries beneath you. Open the
  library's source for that event before writing it: proof-sdk's `markdownLinkClickPlugin` owned
  the read-only view's click (`preventDefault`, `window.open`) in every round, and the sentence was
  never true.
- The reviewer's documentation findings cost a whole round (five threads, a push, the tester and
  the reviewer again) for text alone. The plan's reviewer checklist already said "the AGENTS.md
  passages describe the new behaviour exactly"; treat that line as a per-round check of your own,
  not the reviewer's.

## Evidence (LEGION-695, sjawhar/legion#1928)

Round 1 added the `installTypedBlocks … pictureView` passage to `packages/dispatch/AGENTS.md`,
ending "`ignoreMutation` is always true, since the swap from `<img>` to `<a>` is the one DOM change
it makes itself". Round 1's review removed that override (8af418cf); the sentence stayed through
rounds 2 and 3 and was the first thread of review round 2 (PRRT_kwDORFy7ds6rmCkN). The same
passage and `fallbackLink`'s doc comment said the read-only version view "follows its `href`";
the tester had already noted in round 2 that proof-sdk's plugin opened the page in a new tab
(`.legion/LEGION-695/test.json` round 2, notes). Both were rewritten in af766f05 from
`prosemirror-view/dist/index.js:1110-1111, 3160-3165, 3173-3183` and
`proof-sdk/src/editor/plugins/markdown-link-click.ts` (`handleLinkClick`, `handleMouseMove`), one
round after the code they described had changed.
