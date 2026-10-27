---
title: "A link in the document editor is proven by what window.open receives, on each surface's own click"
category: dispatch-web
tags:
  - playwright
  - proof-editor
  - links
  - e2e
date: 2026-10-27
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-530"
  - "sjawhar/legion#2307"
---

# A link in the document editor is proven by what window.open receives, on each surface's own click

A browser test of a link in a Dispatch document checks two things: the anchor's `href`, and what
the editor's link opener does with that `href`.

- **Record `window.open` and assert the URL it received.** Before the page loads, replace
  `window.open` with a function that saves its URL and returns `null`
  (`page.addInitScript`, `packages/dispatch/e2e/email-links.e2e.ts:71`). Read the saved URLs with
  `page.evaluate`. The test then opens no tab and the result is deterministic. proof-sdk's
  `markdown-link-click.ts` resolves the `href` and passes the result to `window.open` (`:163`).
  Thus a schemeless `href` opens a page path under the document's own URL. An `href` assertion
  alone does not show that.
- **Click as each surface opens a link.** The editable editor opens a link only on a Ctrl or Cmd
  click; a plain click there moves the caret. Use `click({ modifiers: ["ControlOrMeta"] })`
  (`:90`). The read-only past-version view (`getByTestId("version-view")`) opens a link on a
  plain click (`:103`). The modifier click also works in the `iphone` project, so do not skip it
  there.

## Evidence

At the tests-only commit 5158fd92 the `href` of each email link was the bare address. The
recorder received `http://127.0.0.1:18777/issues/CORE-1/second@b.test` from the editor and
`…/issues/CORE-1/artifacts/first@b.test` from the version view: the bare address resolved under
each page's URL. After the fix in 8a431a76 the recorder received `mailto:second@b.test` and then
`mailto:first@b.test`, in both chromium and iphone.
