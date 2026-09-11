# Dispatch Package

Dispatch is the React single-page application for coordinating native Dispatch
issues. The Go server lives in `packages/envoy/cmd/dispatch` and serves the
production build from `web/dist`.

## Layout

- `web/src/app.tsx` owns authentication, the React Router shell, and the responsive sidebar / main content / margin shell. The sidebar holds Inbox, Pinned issues, Projects, and Settings; it never loads the full issue list. The issue main column has Spec, Conversation, Children, and Artifacts tabs; a bare issue route opens the primary Spec document. The project routes `/projects/:key` and `/projects/:key/documents` show a project's grouped, filterable issues and its documents. `/projects/:key/documents/:slug` renders an unlinked project document, including its versions and Referenced by list. The margin holds historical Comments and Pinned for issues; project documents use an artifact owner, so their margin has Comments only, anchors and replies use artifact collaboration routes, and it has no message composer. The Artifacts tab shows the issue's reference closure and each document page shows its inbound references. The three-column layout begins at the `xl` breakpoint (1280px), where the margin is a sticky, full-viewport-height column with its own scrollbar; compact and tablet widths use the navigation drawer and margin bottom sheet, whose review toggle exposes the open-ask count.
- `web/src/api/types.ts` mirrors the Dispatch JSON entities.
- `web/src/features/conversation/` owns the issue's Conversation tab: messages, issue-level comments, and coalesced ask cards from `GET /issues/{key}/events`, with agent titles from `GET /api/v1/agents`. Turns render newest first, with the message composer and its recipient selector sticky above the list and `Load older` below it; a sent or incoming turn appears directly under the composer, an own send always follows even when scrolled into history, and a reader browsing older turns keeps their viewport position as new turns arrive above them.
- `web/src/api/client.ts` is the typed same-origin HTTP client. It is the only
  browser API boundary.
- `web/src/api/sse.ts` opens the issue event stream and invalidates TanStack
  Query cache entries for the affected issue.
- `web/src/main.tsx` installs React Router and the shared Query client.
- `web/src/features/search/` owns the global palette, rail `Search` control, and `Ctrl/Cmd+K` shortcut. It renders server snippets exclusively through `snippetSegments`, never `innerHTML`; document routes pass `?q=` through the document surface to mark and scroll to its first matching rendered text node.

Resolved asks leave the Inbox and open-ask badges, but their thread and Conversation card remain available with the actor and reason. `features/refs/MarkdownBody.tsx` renders every question, answer, comment, reply, and message through Proof's own parser, in every surface that shows one (Inbox, margin, Conversation, and the phone review sheet), not just the Conversation tab; its `inline` variant drops the wrapping block for option labels and clamped previews. The Conversation coalesces each ask lifecycle into one entry with its question, offered options, answer or resolution, and opened/completed timestamps. The margin keeps an anchored resolved ask visible as a closed decision without an answer form.

## Margin threads

The document margin groups each issue-owned anchored root comment and all of its replies into one
flat thread card. A collapsed card shows a two-line root preview and reply summary; selecting it
expands the replies and an inline reply composer. An expanded card and its Proof mark
cross-highlight, and the card aligns to the mark. Project-document thread replies are handled by
the margin-by-owner surface.

Resolved comment threads are hidden until the `Resolved (N)` control is opened. Their cards name
the resolving actor and time, expose `Reopen`, and allow a reply to reopen the thread through the
normal server rule. Only the comment author sees `Edit`; saved edits carry an `edited` marker.
On compact screens the sheet lists thread summaries; opening one presents a full-height thread
view with a bottom-pinned reply composer and Back control.

`AuthGate` resolves `GET /auth/whoami`; unauthenticated visitors see the GitHub
sign-in link at `/auth/start`. Authenticated humans can create native projects
(key + name) and manage external repository-to-project mappings at `/settings`.
All application requests use the same origin so the browser sends the
signed-in cookie.

## Dark mode

Dispatch has no theme toggle: every surface follows the OS `prefers-color-scheme`, which is
Tailwind v4's default `dark:` variant (a `@media (prefers-color-scheme: dark)` query, already
active via `@import "tailwindcss"` in `web/src/styles.css` — there is no `@custom-variant`
override). Components never write a Tailwind color utility literal directly; each imports a
named composite (e.g. `card`, `textPrimaryOnSurface`, `dangerText`) from `web/src/theme/classes.ts`
and interpolates it into `className`. `classes.ts` is the only module where a `dark:`-paired
className string gets built — every export is a static string literal, since Tailwind's
build-time scanner reads source files as plain text and cannot see a class name assembled at
runtime. `web/src/theme/palette.ts` holds the raw OKLCH swatch values (copied from
`tailwindcss`'s own theme, since Tailwind v4 recomputed several hues from their Tailwind v3 hex
constants) and `contrast.ts` is a pure OKLCH→sRGB→WCAG implementation; `classes.ts` is the only
file besides tests allowed to import either. Five tests enforce this: `palette.test.ts` asserts
every registered foreground/background pair meets WCAG AA (4.5:1 text, 3:1 for the focus-ring
UI-component boundary); `no-raw-colors.test.ts` greps every `.ts`/`.tsx` file outside `theme/`
(including `e2e/`) for a raw Tailwind color utility (numbered shades and the `white`/`black`/
`transparent` keywords) or a runtime-concatenated class-name pattern; `classes-in-build-css.test.ts`
builds the app and confirms every token `classes.ts` can produce has a matching rule in the real
output CSS, catching the scanner-blind-spot case those checks exist to prevent;
`text-on-background.test.ts` statically resolves each text composite's nearest enclosing
background composite in its own file and asserts that pairing is registered, since a component
can compose a text role onto a background role its own registration never checked;
`styles-css-pin.test.ts` asserts the hand-written hex/`rgb()` literals in `styles.css` (the
pre-hydration `:root` fallback and Proof editor variables, both outside the `dark:` className
mechanism) equal the exact OKLCH-computed value of the palette swatch their trailing
`/* swatch-name */` comment names. Adding a new color pairing means adding a registered
composite to `classes.ts`, not inventing a shade inline. The document editor
(`@sjawhar/proof-editor`) themes itself through CSS variables scoped to `.proof-editor`;
`styles.css` maps them to palette swatches under `.dispatch-doc .proof-editor` for both schemes,
and those literals are what `styles-css-pin.test.ts` pins.

## Document editor

`features/doc/` adapts `@sjawhar/proof-editor` to Dispatch: it owns the Hocuspocus/Yjs
connection, accessible editor attributes, selected-version presentation, CSS Custom Highlight API
search highlights, and the bridge between Proof marks and margin cards. `DocumentRuntime` supplies
the connection and editor creation seams; happy-dom tests use its doubles from
`web/src/__tests__/document-runtime.ts`, while `e2e/editor.ts` drives the real editor in
Playwright. Library capability gaps belong in `sjawhar/proof-sdk`, not host-side workarounds.

## Commands

Run these from this package:

```bash
bun run dev
bun run build:web
bun run typecheck
bun run lint
bun test
```

## End-to-end tests

`bun run e2e` drives Playwright against the real Go Dispatch server and
Postgres. The harness runs `e2e/run-server.sh` unless
`PLAYWRIGHT_BASE_URL` selects an already deployed server. The harness defaults
to `DISPATCH_E2E_PORT=8777`, which keeps its temporary server separate from
the production listener on port 8766. It defaults `DATABASE_URL` to
`postgres://postgres:dispatch@127.0.0.1:55432/dispatch_c?sslmode=disable` and
uses trusted `X-Dispatch-User` identity for `alice` and `bob`; do not replace it
with a fixture server.

E2E builds set `VITE_DISPATCH_E2E=1`. In that build only, `ProofDocument` exposes its live
`editor` and `view` as `window.__dispatchDocument` for Playwright state probes; production builds
never create that property.

Proof uses collaborative cursor decorations at the desktop `xl` breakpoint and above. Compact
layouts intentionally omit the remote cursor plugin because its edge widget disrupts mobile
post-update text selection; Yjs document transport and local editing remain active.

`e2e/fake-envoy.ts` is a stub Envoy listener the harness starts on
`FAKE_ENVOY_PORT` (default `9021`) and wires through `ENVOY_URL`; tests seed
live sessions with `setLiveSessions` from `e2e/agents.ts`.

`e2e/seed.ts` truncates the test database before each scenario. For a deployed
server, set `PLAYWRIGHT_DATABASE_URL` for the same database and
`E2E_AGENT_TOKEN` for bearer-seeded API calls.

## Phone acceptance

The `iphone` Playwright project uses Chromium with the iPhone 13 viewport,
touch input, and user agent. It verifies the responsive drawer, bottom-sheet
margin, and compact Inbox layout. It does not replace the manual phone check:
run the server on a tailnet-reachable address, open it from a phone, open the
review panel, and answer an open ask in Needs you.
