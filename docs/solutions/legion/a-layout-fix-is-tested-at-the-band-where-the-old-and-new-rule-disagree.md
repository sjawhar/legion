---
title: "A layout fix states its invariant first and is tested at the band where the old and new rule disagree, or each round fixes the previous round"
category: legion
tags:
  - review
  - responsive-layout
  - flexbox
  - regression-lock
  - playwright
  - viewport-sweep
  - invariant-first
date: 2026-09-14
status: active
module: packages/dispatch
related_issues:
  - "LEGION-62"
  - "sjawhar/legion#1088"
symptoms:
  - "a responsive header or toolbar goes through three or more review rounds, each blocking on a different width or interaction"
  - "the regression test added in one round passes at the width it names while the next round's finding is a few pixels away"
  - "a fix for 'the title is squeezed' introduces 'the first click after editing does nothing'"
---

# A Layout Fix States Its Invariant First and Is Tested at the Band Where Old and New Disagree

Pull request #1088 (LEGION-62, the issue header redesign) took five review rounds. Rounds two,
three and four were one thread: each fix for the header title's width below 1280px introduced the
condition the next round blocked on. The sequence, then the two practices that would have made
it one round.

## The sequence

| Round | Finding | Fix | What the fix introduced |
| --- | --- | --- | --- |
| 1 (reviewer) | `grow` on the details line split the row's free space with the title at 1040–1279. | Drop `grow`. | Nothing new — but the title still had `flex-1 min-w-[12rem]`, the *original* squeeze, which the reviewer's short-line 1024 measurement did not reach. |
| 2 (tester) | Just below 1280 the title kept only its 12rem minimum whenever the state controls and details line fit beside 12rem (70–104px title). | Title slot `flex-auto` in view mode, `basis-full` while editing. | A flex-basis that differed between view and edit mode. |
| 3 (reviewer) | The first click on any control after a title edit was swallowed at 768–1279: the blur re-laid out the row between mousedown and mouseup. | One basis in both modes, `basis-full 2xl:basis-auto`. | Nothing; approved next round. |

Each round's regression test locked exactly the width and fixture the finding named — 1024, then
1265/1279 with a busy line, then the mouse sequence — and each passed while the next finding sat
a few pixels or one interaction away. The tests were right; they were written to the symptom.

## Practice 1: write the invariant before touching the classes

Round two's finding was a symptom of a rule nobody had stated: *the title slot has one
flex-basis per breakpoint, independent of what it renders, and siblings share its row only when
they fit beside the whole title*. Round four's fix is exactly that sentence, now a comment above
the slot in `IssueHeader.tsx`. Had it been written in round two, `basis-full` only-while-editing
would have been visibly a violation — a basis that depends on `editingTitle` — and round three
would not have happened.

For a responsive layout fix: before editing a class string, write the rule as one sentence naming
what stays constant across breakpoints, content, and mode; put it in the component comment; then
check every conditional class on the affected flex items against it. A class that flips on focus,
blur, hover, or an editing flag is suspect the moment the rule mentions "one basis" or "same row".

## Practice 2: sweep the band, on both builds, with a real pointer

The three measurable claims in this PR — header height, title width, page scroll width — all
changed at breakpoints, and every regression lived in the band just below a breakpoint where the
old and the new rule broke lines differently. What found the rounds' bugs was always a
measurement sweep, never a read of the JSX:

- widths at `1024, 1200, 1230, 1260, 1265, 1271, 1279, 1280, 1536` — the two `xl` neighbours plus
  the band a 1280 screen with a scrollbar actually renders at (about 1265);
- the same fixture on the head **and on main** (swap the changed component files into the same
  bundle and rebuild), so "at or above main" is a number, not a claim;
- a busy details line **and** a bare one — the busy line with a GitHub link was too wide to share
  a row and never showed the squeeze; the line without the link did;
- one real pointer interaction per control that closes an edit (`page.mouse.down()`/`up()`, not
  `locator.click()`), because a layout that moves on blur is invisible to synthetic clicks.

The Playwright harness makes this cheap: a throwaway spec that loops viewports and logs
`JSON.stringify` of the boxes runs in seconds against the real Go server; delete it before the
push and keep only the assertions a plausible relapse would fail. The final locks in
`e2e/issue.e2e.ts` (title width at 1024/1200/1279 and 1265/1279 busy; mouse-down/up on the pin
and Close at 1024/1279) and `e2e/labels.e2e.ts` (scroll container and page width at 1280/390) are
the distilled sweep.

## One harness fact that costs time

- `main`'s `IssueHeader.tsx` cannot be shown with `jj file show -r main@origin <path>` from inside
  `packages/dispatch`; jj resolves the path against the workspace root, so run it from
  `$LEGION_WORKSPACE`.

## Related

- `docs/solutions/dispatch-web/a-flex-slot-that-swaps-content-must-keep-one-basis-and-a-flex-row-breaks-on-hypothetical-widths.md`
  — the flexbox mechanics behind the three rounds.
- `docs/solutions/testing/a-query-error-branch-is-asserted-after-the-query-reaches-error-not-after-the-mock-is-called.md`
  — the unit-test timing finding from the same review.
