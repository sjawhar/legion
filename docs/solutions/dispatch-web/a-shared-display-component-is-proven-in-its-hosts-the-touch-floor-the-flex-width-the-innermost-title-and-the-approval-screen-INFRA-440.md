---
title: "A shared display component is proven in its hosts: the touch floor, the flex width, the innermost title, and the approval screen"
category: dispatch-web
tags:
  - shared-component
  - flexbox
  - touch-target
  - tooltip
  - untrusted-input
  - lint-guard
  - phone-layout
date: 2026-10-26
status: active
module: packages/dispatch/web/src/features/refs/AgentName.tsx
related_issues:
  - "INFRA-440"
  - "sjawhar/legion#2237"
---

# A shared display component is proven in its hosts: the touch floor, the flex width, the innermost title, and the approval screen

When one component replaces a value's hand-drawn markup in many places, its unit tests and its
own acceptance rows pass while the defects sit at the joins with each host. Before calling such a
cutover done, check each of these against the hosts, not the component:

- **Its own box never widens a host.** Give the component's outer element `min-w-0
  [overflow-wrap:anywhere]`. A flex item's default `min-width: auto` turns one long unbroken string
  (an id, a snake_case title) into page overflow in whichever host lacks `min-w-0`. Seed a long
  string with no spaces and check `scrollWidth === clientWidth` at 390 px in a host you did not
  write. A host that wants one line keeps its own `truncate`; nowrap still ellipsizes.
- **Every standalone link or button it adds pays the touch floor.** Below 1280 px `styles.css`
  gives every non-inline anchor and every button a 44 px minimum. Adding one beside text in a
  `whitespace-nowrap` row takes 44 px from that text. Add up the row's widths at 390 px, and assert
  the text is still readable (`scrollWidth <= clientWidth` on the text element). "The page is not
  wider" and "nothing overlaps" both pass while the name reads `Le…`. A row that cannot fit wraps
  below a width with `@container` and `@max-sm:` utilities.
- **A tooltip on the component is overridden by any title inside it.** The browser shows the
  innermost element's `title`, and `components/TruncatedText.tsx` always sets one. A host that
  wraps the name in its own control (a render prop) and truncates it there hides the component's
  tooltip. Put the full value in that host's own title too, and assert the hover text on the
  innermost titled element.
- **A compact form never stands alone where a person decides trust.** Where the value is an
  identifier the requester asserts (a credential request's session id is an unsigned claim), show
  it in full as text, and the state in words, not only as a prefix plus an icon whose meaning sits
  in an `aria-label`. `session:<first 8>…` is shared by UUIDv7 ids minted in the same minute, and a
  forged id can pick it. When a cutover pushes the compact form everywhere, list the approval and
  credential screens it reaches and check each one.
- **One rule, no reverse parsing.** When the component needs a part of a composed label, export
  that part from the module that composes it (`actorAttribution`, with
  `actorLabel = actorName + actorAttribution`) and route every naming path through one resolver.
  Never slice the composed string. A component that takes a host's render prop calls it on every
  branch, including kinds no current caller passes. A link's accessible name is its visible text;
  an `aria-label` that repeats the text after the link makes a screen reader read it twice.
- **An import ban is one half of the guard.** Biome's `style/noRestrictedImports` catches named,
  aliased and re-exported imports, and relative specifiers through `patterns` (its
  `importNamePattern` refuses `^`/`$` anchors). It does not catch a namespace import with member
  access, or a hand-written copy of the rule. Commit an `rg` audit with its expected hits beside it,
  prove the ban with throwaway files of each import form, and add each new export of the owning
  module to both the ban and the audit.

## Evidence

All from sjawhar/legion#2237, which moved about 38 places onto `AgentName`:

- The tester's red row: a 40-character registry title with no space on a Conversation turn and
  reply made the page 530 px wide at 390 px. The byline was a flex item with no `min-w-0`. Fixed
  once, in the component (`AGENT_SPAN_CLASS`), so every host got it. With those classes removed
  the row fails again at 530 px.
- Review round 1: the Inbox ask card's provenance row is 292 px and nowrap. The `control`
  variant's `↗` link took 44 px, leaving the asker's button about 48 px, so the name read `Le…`.
  The plan's phone checks were no widening and no overlap, and both passed. A new iphone row
  asserting the name is not elided failed (`scrollWidth` 144 > `clientWidth` 28) until
  `AskCard.tsx` wrapped the turn label onto its own line below 24rem.
- Review round 1 (Security): credential approval lines went from `<full id> isn't running.` to
  `session:<first 8>…` plus an 8 px ring. A browser probe with a forged id sharing a real session's
  first 8 characters now reads `session:019a2b3c… · 019a2b3c-forged-claim-9999 isn't running.`.
- Review round 2: the component's `title={sessionId}` on the short form did not reach the Inbox
  asker, because the host's `TruncatedText title={label}` sits inside it.
- Review round 1: the attribution came from `label.slice(name.length)`; `kind="control"` with a
  user actor dropped the host's button; the link's `aria-label` repeated the attribution. The lint
  guard did not see `ConversationTab`'s `sessionTitle()`, which named agents by raw title; the
  review found it by reading.
