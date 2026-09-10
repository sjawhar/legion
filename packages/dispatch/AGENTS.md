# Dispatch Package

Dispatch is the React single-page application for coordinating native Dispatch
issues. The Go server lives in `packages/envoy/cmd/dispatch` and serves the
production build from `web/dist`.

## Layout

- `web/src/app.tsx` owns authentication, the React Router shell, and the responsive sidebar / main content / margin shell. The issue main column has Spec, Log, Children, and Artifacts tabs; the review margin holds Comments and Pinned. The three-column layout begins at the `xl` breakpoint (1280px); compact and tablet widths use the navigation drawer and margin bottom sheet.
- `web/src/api/types.ts` mirrors the Dispatch JSON entities.
- `web/src/api/client.ts` is the typed same-origin HTTP client. It is the only
  browser API boundary.
- `web/src/api/sse.ts` opens the issue event stream and invalidates TanStack
  Query cache entries for the affected issue.
- `web/src/main.tsx` installs React Router and the shared Query client.

Resolved asks leave the Inbox and open-ask badges, but their thread and log entry remain available with the actor and reason. The margin keeps an anchored resolved ask visible as a closed decision without an answer form.

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
pre-hydration `:root` fallback and the CodeMirror anchor overlays, both outside the `dark:`
className mechanism) equal the exact OKLCH-computed value of the palette swatch their trailing
`/* swatch-name */` comment names. Adding a new color pairing means adding a registered
composite to `classes.ts`, not inventing a shade inline. The document editor (`DocEditor.tsx`)
additionally opts a CodeMirror `Compartment` into `{ dark: true }` when the media query matches,
since CodeMirror's own base theme otherwise hardcodes a light caret and selection color
regardless of the page's color scheme.

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

`e2e/seed.ts` truncates the test database before each scenario. For a deployed
server, set `PLAYWRIGHT_DATABASE_URL` for the same database and
`E2E_AGENT_TOKEN` for bearer-seeded API calls.

## Phone acceptance

The `iphone` Playwright project uses Chromium with the iPhone 13 viewport,
touch input, and user agent. It verifies the responsive drawer, bottom-sheet
margin, and compact Inbox layout. It does not replace the manual phone check:
run the server on a tailnet-reachable address, open it from a phone, and answer
an open ask from the Inbox.
