---
title: "A second path onto an existing page is taught to every pathname predicate, and the router's ranking is repeated by hand"
category: dispatch-web
tags:
  - react-router
  - routing
  - splat
  - app-shell
  - deletion
date: 2026-11-02
status: active
module: packages/dispatch/web
related_issues:
  - "sjawhar/legion#2649"
---

# A second path onto an existing page is taught to every pathname predicate, and the router's ranking is repeated by hand

Extends docs/solutions/dispatch-web/a-page-component-mounted-in-a-second-surface-names-every-page-wide-claim-it-makes-pr2549.md.
That rule is for a page's component drawn in a second surface; this one is for the same page drawn
at a second **path**, where the component is untouched and the shell decides its chrome from
`location.pathname`.

- **After you change `app.tsx`'s `<Route>` list, sweep `web/src` for every predicate that takes a
  `pathname` and decide each one explicitly.** The output of the sweep is a decision per
  predicate, not a diff: in sjawhar/legion#2649, `routeIsInbox` had to change and `routeHasMargin`
  and `routeFillsViewport` had to stay, and saying so is the work. A page the shell does not
  recognise still renders; it renders with another page's chrome, which no type and no router
  error reports.
- **A predicate that mirrors a splat repeats the router's ranking itself.** React Router ranks a
  concrete path above a splat in the same `<Routes>`, so `/credentials/machine` keeps its own page
  under `/credentials/*`. `matchPath` does no such ranking: `matchPath("/credentials/*",
  "/credentials/machine")` matches, so a predicate written as that one call alone claims the
  machine page is the Inbox. Exclude the concrete siblings in the predicate, in the same order the
  router applies them.
- **Pin the predicate with a table of paths, not with one example.** Assert `[pathname, result]`
  tuples so a failure names the path it disagreed about, and put the concrete sibling in the false
  list beside the splat's own paths.
- **One mirror of the router is enforced and the others are not.** `e2e/two-builds-routes.ts` is
  held to `app.tsx` by a regex in `e2e/__tests__/two-builds-routes.test.ts`, so a route added
  there fails that test until the list agrees; nothing holds the pathname predicates to anything.
  Do not read a green route-table test as "the routing change is complete".
- **Rendering an existing page at a retired page's path is a way to retire a path**, and it keeps
  `app.tsx` and the route-table mirror comparable: the mirror's regex matches `<Route element={<Name
  />} path="…" />` and nothing else, so `<Navigate replace to="/" />` would silently drop out of
  the comparison the mirror exists to make.

## Evidence (sjawhar/legion#2649, INFRA-879)

The change deleted Dispatch's credential record page and routed its path at the Inbox:
`packages/dispatch/web/src/app.tsx:808-809` reads `<Route element={<InboxPage />}
path="/credentials/*" />` above `<Route element={<MachineLoginPage />} path="/credentials/machine"
/>`, with react-router 6.30.6 ranking the concrete path first.

`routeIsInbox` (`packages/dispatch/web/src/features/refs/routes.ts:944`) still read `pathname ===
"/"`. Its four readers are `app.tsx:434` (whether the shell gives the page the viewport, paired
with the reading pane), `app.tsx:508`, `app.tsx:687` and `Sidebar.tsx:149` (whether to offer a peek
at the Inbox). So the Inbox at `/credentials/<record>` rendered without its own scroller and
offered the reader a peek at the page they were already on. The route-table mirror test was green
across that whole commit; the repository's `ce-simplify-code` pass is what found it.

The fix repeats the router's ranking:

```ts
export function routeIsInbox(pathname: string): boolean {
  return (
    pathname === "/" ||
    (matchPath("/credentials/*", pathname) !== null &&
      matchPath("/credentials/machine", pathname) === null)
  );
}
```

pinned by `routes.test.ts:892`, a table whose true list is `/`, `/credentials/record-old-broker`
and `/credentials`, and whose false list opens with `/credentials/machine`.
`routeHasMargin` (`routes.ts:933`) and `routeFillsViewport` (`routes.ts:969`) were read and left
alone: the Inbox owns no margin and fills the viewport only through `routeIsInbox`.
