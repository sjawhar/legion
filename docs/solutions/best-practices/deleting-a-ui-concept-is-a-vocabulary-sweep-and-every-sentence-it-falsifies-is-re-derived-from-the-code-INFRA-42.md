---
title: "Deleting a UI concept is a vocabulary sweep, and every sentence it falsifies is re-derived from the code"
category: best-practices
tags:
  - documentation-edits
  - agents-md
  - code-comments
  - cutover
  - review-rounds
  - dispatch-web
date: 2026-10-20
status: active
module: packages/dispatch, packages/envoy/AGENTS.md, docs/site, skills
related_issues:
  - "INFRA-42"
  - "INFRA-67"
  - "sjawhar/legion#2086"
---

# Deleting a UI concept is a vocabulary sweep, and every sentence it falsifies is re-derived from the code

When a change removes a concept a reader can name — a view, a band, a stored preference, a URL
parameter — the prose that describes it is part of the change, and two different mistakes make it
outlive the code. Do both of these before the first push.

- **Sweep the vocabulary, not the diff's hunks.** Grep the whole tree for the removed words,
  including as substrings inside prose, not only as the terms a plan listed:
  `Mine`, `Everyone`, `the Everyone view`, `into Mine`, `Mine and Unassigned`, `Unassigned band`,
  `inbox.view`, `InboxView`, `?view=`, `onViewChange`. Search `packages/**`, `docs/**` and
  `skills/**` — another package's `AGENTS.md` is auto-loaded into every session started in this
  repository, so a deleted contract described there is read as current by the next agent.
  Grepping only the plan's terms finds the headings and misses the sentence that mentions the
  concept in passing, which is the one that survives.
- **Re-derive every sentence the change falsifies from the code, not from the sentence it
  replaces.** A sentence that was true because of the thing being deleted becomes false when it
  goes, and rewording it carries the claim across in new words that no grep can find: the
  vocabulary is gone and the false statement is not. Read the new sentence against the code it
  describes and against the tests that exercise that code. A comment that contradicts the branch's
  own browser test is the cheapest possible catch, and it is a grep away once you know the claim.
- **State one rule in one sentence and paste it.** Where the same rule appears in a comment, an
  `AGENTS.md`, a documentation page and a test, write the sentence once and copy it; a paraphrase
  in the second place is how two pages of the same pull request came to contradict each other.
- **When you correct such a sentence, grep for its siblings.** The claim usually exists in more
  than one file, written in the same push by the same hand; correcting one and not the others is
  the defect reproducing itself.

## Evidence (INFRA-42, sjawhar/legion#2086)

The change replaced the Dispatch Inbox's `Mine`/`Everyone` views with one page filtered by
`?assignee=`, and removed the `Unassigned` band and the `inbox.view` preference. Three of four
review rounds were spent on prose, every instance outside the diff's hunks:

- Round 1: `packages/envoy/AGENTS.md:1811` still documented the Mine/Everyone split, the
  `Unassigned` band, `inbox.view` and `?view=` as the SPA's contract; a false claim about an
  Agents pill appeared in both `AgentsPage.tsx`'s comment and
  `docs/site/src/content/docs/dispatch/agents.md`; `BulkSnoozeBar.tsx:50` and
  `keyboard-inbox.e2e.ts:622` navigated by the removed switch.
- Round 2: three more comments did (`Inbox.tsx:519`, `SnoozeControl.tsx:20`,
  `useIssueAssignee.ts:23`), found only because the reviewer swept the words the first sweep had
  missed as substrings.
- Round 3: `Inbox.tsx:518-519`, rewritten in round 2, now claimed "Assign to me is likewise not a
  hold". Under `?assignee=unassigned` — the one filter that offers the control — the settled
  assignment drops the row out of `shown` (`Inbox.tsx:460-462`), `listed` is undefined
  (`:692-693`) and `heldRow` returns `seen` (`:286-295`): the row is held until the reader's hand
  leaves it. The branch's own browser test says so and reloads for exactly that reason
  (`packages/dispatch/e2e/inbox.e2e.ts:1391-1397`), and the implementer had written that test
  comment in the same round as the false sentence.

The same rewrite push also produced the twin at `useIssueAssignee.ts:22-23` ("moves in or out of
the list at once"), which is false in the out direction for the same reason. It was still there
after the reviewer's approval, found by the retro's fresh-eyes pass: the correction had reached
one sibling and not the other, and three review rounds, two thermonuclear pairs and a merge audit
had passed over it. It is filed as `dispatch://INFRA-67` with `packages/envoy/AGENTS.md:1810` —
a sentence one line above the line review round 1 made this same pull request fix, in the same
auto-loaded file, which still names the band this change renamed. Both are this change's residue,
recorded rather than fixed because they were found after a human-approved head existed: correcting
them there would have cost a backward move to implementing, a test round, a review round and
another retro, to change two sentences no user of the product can see.

That is the sharpest form of the rule above. When you correct a sentence your change falsified,
grep its claim's words across the tree before you call it done — most of all in the file you are
already editing, where its siblings were written by the same hand in the same push.
