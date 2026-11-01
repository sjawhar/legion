---
title: "A layout change invalidates the documentation shots whose readiness waits on the old shape"
category: testing
tags:
  - docs-site
  - screenshots
  - playwright
  - ci
date: 2026-10-31
status: active
module: docs/site/media
related_issues:
  - "sjawhar/legion#2549"
---

# A layout change invalidates the documentation shots whose readiness waits on the old shape

A change to what a page draws breaks every documentation screenshot whose readiness helper waits for
an element of the old shape, and that failure arrives as a red `docs / build` job rather than as a
test of the feature. Before you push a layout change:

- Grep `docs/site/media/*/shots.config.ts` and `docs/site/media/shots.config.ts` for the page you
  changed, and read each `ready` helper: one that waits by role for an element your change moves
  needs splitting, one helper per shape the page now has.
- Give a shot that must show the other shape its own `size`, rather than changing the page: the shot
  runner already honours a per-shot viewport.
- Re-run the shots and the site build before the push (`bun docs/site/media/shots.ts`, then
  `cd docs/site && bun run build`), and read the prose beside each shot: a caption that describes the
  old shape is as wrong as the picture.

## Evidence

The `docs / build` job of CI run 41128214713 failed with `4 shot(s) failed`: `dispatch/inbox`,
`dispatch/ask-card`, `dispatch/shortcuts` and `legion/spec-approval` each waited on
`getByRole('article', { name: 'Urgency: Blocking' })`, the Inbox's card, which from 1280 px this
change replaces with a short row and a reading column. Nothing in the plan named those shots; CI
alone found them.

The fix split `inboxReady` into `inboxPaneReady` and `inboxCardsReady`
(`docs/site/media/shots.config.ts:12`, `:22`) and added `size?: { width, height }` to `Shot`
(`docs/site/media/shot-runner.ts:34`), so `ask-card` is taken at a width where the card is still what
the Inbox draws. Four pages of prose moved with the pictures
(`docs/site/src/content/docs/dispatch/inbox-and-asks.md`, `index.md`, `keyboard.md`,
`legion/walkthrough.md`), and the re-run took all 22 shots with `exit 0`, with the site building
`43 page(s)` and every internal link valid.
