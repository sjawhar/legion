# Dispatch Package

Dispatch is the React single-page application for coordinating native Dispatch
issues. The Go server lives in `packages/envoy/cmd/dispatch` and serves the
production build from `web/dist`.

## Layout

- `web/src/app.tsx` owns authentication, the React Router shell, and the responsive sidebar / main content / margin shell. The sidebar holds Inbox, Pinned issues, Projects, and Settings; it never loads the full issue list. The issue main column has Spec, Conversation, Children, and Artifacts tabs; a bare issue route opens the primary Spec document. The project routes `/projects/:key` and `/projects/:key/documents` show a project's grouped, filterable issues and its documents. `/projects/:key/documents/:slug` renders an unlinked project document, including its versions and Referenced by list. The margin holds historical Comments and Pinned for issues; project documents use an artifact owner, so their margin has Comments only, anchors and replies use artifact collaboration routes, and it has no message composer. The Artifacts tab is a compact, filterable list of the issue's artifacts (kind, version count, last updated, and a download link for the latest version), sorted primary-first then most recently updated, with the issue's reference closure collapsed behind a disclosure; each artifact's own page (`/issues/:key/artifacts/:slug`) carries its version history, a From/To version compare, and its inbound references. The three-column layout begins at the `xl` breakpoint (1280px), where the margin is a sticky, full-viewport-height column with its own scrollbar; compact and tablet widths use the navigation drawer and margin bottom sheet, whose review toggle exposes the open-ask count.
- `web/src/api/types.ts` mirrors the Dispatch JSON entities.
- `web/src/features/conversation/` owns the issue's Conversation tab: messages, issue-level comments, and coalesced ask and targeted-message cards from `GET /issues/{key}/events`, with live agent titles and capabilities from `GET /api/v1/agents`. The sticky Message composer preselects the issue route when present, otherwise opens a **To** picker with roles before sessions; it offers **BTW**, **Aside**, and **Steer**, disabling unadvertised modes. Targeted cards coalesce delivery attempts and replies, show failure text, and preserve **Ask BTW again** / **Send normally** until an answer arrives; all recipient and retry controls remain touch-sized. Turns render newest first, with `Load older` below them; a sent or incoming turn appears directly under the composer, an own send always follows even when scrolled into history, and a reader browsing older turns keeps their viewport position as new turns arrive above them.
- The issue header's Subscribed agents section (`features/issue/SubscribedAgents.tsx`) and the equivalent block on a project document page list sessions from `GET .../subscribers`, live status merged from Envoy, and a human `Unsubscribe` action that notifies the removed session.
- `features/issue/IssueLabels.tsx` edits issue-header labels with project-label suggestions; `IssueList.tsx` filters project issues by every selected label through URL-backed, repeatable `?label=` parameters.
- `web/src/api/client.ts` is the typed same-origin HTTP client. It is the only
  browser API boundary.
- `web/src/api/sse.ts` opens the issue event stream and invalidates TanStack
  Query cache entries for the affected issue.
- `web/src/main.tsx` installs React Router and the shared Query client.
- `web/src/features/search/` owns the global palette, rail `Search` control, and `Ctrl/Cmd+K` shortcut. It groups hits by their issue or standalone project-document owner; document-owned hits use the document name and `/projects/:key/documents/:slug` route, adding an ask or comment query parameter for discussion hits. It renders server snippets exclusively through `snippetSegments`, never `innerHTML`; document routes pass `?q=` through the document surface to mark and scroll to its first matching rendered text node.

Project issue routes default to the persisted List view and offer a keyboard-reachable Board view. Each signed-in user's List/Board choice is retained separately in the browser; board columns follow the lifecycle, card position is the issue's persisted `rank`, and humans can reorder cards or move them only into `triage`, `icebox`, `backlog`, and `todo`.

Resolved asks leave the Inbox and open-ask badges, but their thread and Conversation card remain available with the actor and reason. `features/refs/MarkdownBody.tsx` renders every question, answer, comment, reply, and message through Proof's own parser, in every surface that shows one (Inbox, margin, Conversation, and the phone review sheet), not just the Conversation tab; its `inline` variant drops the wrapping block for option labels and clamped previews. The Conversation coalesces each ask lifecycle into one entry with its question, offered options, answer or resolution, and opened/completed timestamps. The margin keeps an anchored resolved ask visible as a closed decision without an answer form.

A document's approval (`features/doc/ApprovalChip.tsx`) is a human review pinned to a version: the header of an issue document, the issue's Spec header, and a project document page show a chip (`Draft` header-only, `Awaiting approval`, `Approved v12`, `Approved v12 · changed since` when edited after approval, `Changes requested`) that opens the review history, plus `Approve` (`Approve v<latest>` when stale) and `Request changes` (reason required) controls that `POST /artifacts/{id}/reviews`; artifact and document lists show the chip only, never for a draft. An agent's `dispatch_request_approval` opens an ask of `kind: "approval"`, which the Inbox card renders as `Approval requested` with the two fixed options and no Other row, requiring a `Reason` when `Request changes` is chosen; answering it is the same review. Approval is the exception path (Legion's design gate), so nothing about it is prominent.

Answering and asking for clarification are different acts on an ask card (`features/inbox/`). Answering: when an ask offers
options, free text is an explicit **Other** choice - the last row of the option list, which reveals the `Your answer` field;
a question-shaped Other response presents an inline default action to send it as clarification (keeping the ask open) or to answer
with it anyway. A chosen Other is recorded as `{selected: [], text}` (single) or `{selected, text}` (multiple) and renders under an
`Other` label once answered. An ask with no options keeps the free-text field alone. Clarifying: the reply thread under an open ask
is labelled `Ask for clarification` / `Send` (answered asks keep `Reply`) and says `Replying does not answer the question.` The Inbox
partitions the server-ordered rows by `last_reply`: asks whose newest reply is a human's are listed under `Waiting on agents`
with a `Waiting on <asker>` chip (the agent owes the next turn), everything else under `Needs you` with a `<agent> replied` chip
when an agent spoke last; both headings appear only when the waiting section is non-empty. The card, its collapsed disclosure,
and its inline thread share one `["ask-thread", id]` query of `GET /api/v1/asks/{id}`, whose `edits` list every rewording; the
card's `Show N previous versions` disclosure renders each with its editor and time.

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
sign-in link at `/auth/start`. Authenticated humans can create native projects,
manage external repository-to-project mappings, and mint or revoke personal agent
tokens at `/settings`. A personal token's session writes display the session title
followed by `(for <owner>)`. All application requests use the same origin
so the browser sends the signed-in cookie.

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
Live document block links use `#b-<blockId>`: once Proof is ready, Dispatch focuses and pulses that stable block. Copying a document block link uses the selected block's `blockId`; historical versions stay read-only markdown views.

Before constructing Proof, Dispatch fetches `/api/v1/schema/blocks` once and keeps the schema by
version for the session. It passes that schema to the live editor, historical-version editor, and
the one headless Markdown engine cached per schema version; the connection carries it as
`schema_version`. A server version mismatch admits the tab read-only and the surface shows
`Reload to edit`. Typed blocks use `:::name{#block-id key="value"}` container directives. The server
owns their schema and version, while Dispatch owns host rendering for `render: "host"` types; the
SPA must not invent node schemas or parse a second directive grammar.

`ask` is the host-rendered decision type. Every live document shows its open ask-block decisions
as `#b-<blockId>` links; the in-document form and Inbox submit the same answer route, and an
answered block is read-only. The editor library supplies its schema-aware Insert and Turn into
block-menu entries; Dispatch passes the server schema rather than duplicating those commands.


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
`FAKE_ENVOY_PORT` (default `9021`) and wires through `ENVOY_URL`. Tests seed
live sessions and their capabilities with `setLiveSessions`, change one
session's liveness or scripted 200/404 send response with `setSessionLive` /
`setSessionSendStatus`, and inspect targeted sends with `getSentMessages`;
persisted subscriptions use `setInterests`, all from `e2e/agents.ts`.

`e2e/seed.ts` truncates the test database before each scenario. For a deployed
server, set `PLAYWRIGHT_DATABASE_URL` for the same database and
`E2E_AGENT_TOKEN` for bearer-seeded API calls.

## Phone acceptance

The `iphone` Playwright project uses Chromium with the iPhone 13 viewport,
touch input, and user agent. It verifies the responsive drawer, the margin
and recipient-picker bottom sheets, touch-sized controls, and compact Inbox
layout. It does not replace the manual phone check: run the server on a
tailnet-reachable address, open it from a phone, open the review panel, and
answer an open ask in Needs you.
