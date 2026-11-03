---
title: "An unlayered rule in styles.css needs its own dark twin, because no Tailwind dark: utility can override it"
category: dispatch-web
tags:
  - css
  - cascade-layers
  - tailwind
  - typography
  - dark-mode
date: 2026-10-27
status: active
module: packages/dispatch/web
related_issues:
  - "INFRA-527"
  - "sjawhar/legion#2322"
---

# An unlayered rule in styles.css needs its own dark twin, because no Tailwind dark: utility can override it

Extends docs/solutions/testing/a-line-pitch-is-read-on-the-mounted-route-and-a-pixel-figure-names-its-font-and-rounding-INFRA-304.md.

That document says a declaration in no cascade layer beats every layered one. Two consequences
for `packages/dispatch/web/src/styles.css`:

- **A value an unlayered rule sets is not changed by a `dark:` class on the same element**, a
  custom property included (`dark:prose-invert` sets `--tw-prose-links` inside a layer). When the
  value must change in dark mode, write the twin yourself under
  `@media (prefers-color-scheme: dark)` next to the rule, as the file already does for its other
  hand-written colours. This works because Dispatch's `dark:` is that media query, with no theme
  toggle (`styles.css`, the comment above `color-scheme: light dark`).
- **A read-only renderer marks its own elements with its own attribute** (for example
  `data-markdown-task-checkbox`), never the editor's classes or attributes
  (`proof-task-checkbox`, `data-task-checkbox`). Proof-editor's `lib.css` is unlayered, loads when
  the first editor mounts and stays for the session, and its `.proof-task-checkbox` rule
  (`packages/proof-editor/src/lib.css`) would then style a renderer's box anywhere in the page.

Prove a colour per scheme in the browser (`page.emulateMedia({ colorScheme })`). Poll the
computed colour (`expect.poll`), because links and buttons ease `color` over 150 ms after a scheme
switch (`expectTextColor` in `e2e/theme.e2e.ts`).

## Evidence

- `.dispatch-markdown` (`styles.css`) set `--tw-prose-links: #0069a8` and
  `--tw-prose-invert-links: #00bcff`, unlayered, since sjawhar/legion#919. The element also
  carries `dark:prose-invert` (`features/refs/markdown-engine.ts`). So in dark mode
  `--tw-prose-links` stayed sky-700, and every link in a Markdown body rendered `rgb(0, 105, 168)`
  on a dark card. The app's own link role is `text-sky-700 dark:text-sky-400`
  (`theme/classes.ts`, `linkText`).
- INFRA-527 added `@media (prefers-color-scheme: dark) { .dispatch-markdown { --tw-prose-links:
  var(--tw-prose-invert-links); } }`. The new `theme.e2e.ts` row reads `rgb(0, 188, 255)` in dark
  at the head. At the fork point 7f7edb8d it failed: `Expected "rgb(0, 188, 255)", Received
  "rgb(0, 105, 168)"`.
- A first read without polling returned `rgb(0, 134, 198)`, a colour partway through the 150 ms
  transition.
