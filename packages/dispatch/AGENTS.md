# Dispatch Package

Dispatch is the React single-page application for coordinating native Dispatch
issues. The Go server lives in `packages/envoy/cmd/dispatch` and serves the
production build from `web/dist`.

## Layout

- `web/src/app.tsx` owns authentication, the React Router shell, and the responsive sidebar / main content / margin shell. The sidebar holds Inbox, Pinned issues, Projects, Agents, and Settings; it never loads the full issue list. The `/agents` route lists live Envoy sessions with their open-ask counts, last activity, and freshness; its targeted **BTW**/**Steer** composer requires a selected issue. The issue main column has Spec, Conversation, Children, and Artifacts tabs; a bare issue route opens the primary Spec document. The project routes `/projects/:key` and `/projects/:key/documents` show a project's grouped, filterable issues and its documents. `/projects/:key/documents/:slug` renders an unlinked project document, including its versions and Referenced by list. The margin holds historical Comments and Pinned for issues; project documents use an artifact owner, so their margin has Comments only, anchors and replies use artifact collaboration routes, and it has no message composer. The Artifacts tab is a compact, filterable list of the issue's artifacts (kind, version count, last updated, and a download link for the latest version), sorted primary-first then most recently updated, with the issue's reference closure collapsed behind a disclosure; each artifact's own page (`/issues/:key/artifacts/:slug`) carries its version history, a From/To version compare, and its inbound references. The three-column layout begins at the `xl` breakpoint (1280px), where the sidebar and sticky, full-viewport-height margin can each collapse into a discoverable slim rail; the main column then claims all remaining width. Each signed-in user's sidebar and margin visibility, and the margin's 280px-to-60vw resize width, persist in browser storage. Compact and tablet widths use the navigation drawer and margin bottom sheet, whose review toggle exposes the open-ask count.
- `web/src/api/types.ts` mirrors the Dispatch JSON entities.
- `web/src/features/conversation/` owns the issue's Conversation tab: messages, issue-level comments, and coalesced ask and targeted-message cards from `GET /issues/{key}/events`, with live agent titles and capabilities from `GET /api/v1/agents`. The sticky Message composer preselects the issue route when present, otherwise opens a **To** picker with roles before sessions; it offers **BTW**, **Aside**, and **Steer**, disabling unadvertised modes. Targeted cards coalesce delivery attempts and replies, show failure text, and preserve **Ask BTW again** / **Send normally** until an answer arrives; all recipient and retry controls remain touch-sized. Turns render newest first, with `Load older` below them; a sent or incoming turn appears directly under the composer, an own send always follows even when scrolled into history, and a reader browsing older turns keeps their viewport position as new turns arrive above them.
- `features/issue/IssueHeader.tsx` keeps issue identity, state controls, and metadata in one dense wrapping header: at available width, status/priority, approval actions, labels, route, subscribers, and external links share a row; narrow layouts wrap them without horizontal scrolling. The title is two-line clamped, desktop state controls are compact, and phone controls remain touch-sized. Its `Subscribers: N` control opens `features/issue/SubscribedAgents.tsx`, whose list merges persisted subscriptions with Envoy liveness and offers a human `Unsubscribe` action that notifies the removed session.
- `features/issue/IssueLabels.tsx` edits issue-header labels in a searchable multi-select popover: it combines the issue's labels with labels used anywhere in the project, stages changes locally, and saves the final draft when the popover closes; `IssueList.tsx` filters project issues by every selected label through URL-backed, repeatable `?label=` parameters.
- `web/src/api/client.ts` is the typed same-origin HTTP client. It is the only
  browser API boundary.
- `web/src/api/sse.ts` opens the workspace event stream. Its shared cache-key vocabulary refreshes issue, ask/thread, comment, artifact-reference, project-document, subscriber, and settings data for the matching event; reconnect invalidates every live-data family before the UI renders stale data.
- `web/src/main.tsx` installs React Router and the shared Query client.
- `web/src/features/search/` owns the global palette, rail `Search` control, and `Ctrl/Cmd+K` shortcut. It groups hits by their issue or standalone project-document owner; document-owned hits use the document name and `/projects/:key/documents/:slug` route, adding an ask or comment query parameter for discussion hits. It renders server snippets exclusively through `snippetSegments`, never `innerHTML`; document routes pass `?q=` through the document surface to mark and scroll to its first matching rendered text node.

The primary document's `Version`, `Name version`, connection dot, and block-link controls live at the end of the active Spec tab row; on phones they wrap below the tabs. `Conversation`, `Children`, and the artifact list do not render those primary-Spec controls.

Project pages keep their name, linked project key, project tabs, and, on issue routes, the keyboard-reachable List/Board control in a dense responsive header. At `md` and above these controls form one strip; on smaller screens the name, key, and blocker pill form the first row, with tabs and the view control below. When an open ask waits on the viewer, the header's `Blocked on you · N` pill links to the Inbox; it is absent when no asks are waiting.

Project issue routes default to the persisted List view and offer a keyboard-reachable Board view. Each signed-in user's List/Board choice is retained separately in the browser; the server orders project and pinned lists by lifecycle, then `P0`–`P3` priority with unset issues last, then persisted `rank`, while each board column reorders its cards by `rank` so drag position stays authoritative. List rows and board cards show priority badges, while the issue header offers a compact priority select beside Status. Humans can reorder cards or move them into any open lifecycle status. Dropping a card into `done` closes it; dragging it from `done` into any open lifecycle column reopens it. An open issue header's Status select offers every open lifecycle status; **Close issue** is the only open path into Done, while the closed-issue callout offers **Reopen** into Backlog.

Project List filters live in a per-login `project.issue-filters` browser preference. With no active filters the disclosure starts collapsed on every viewport; its trigger reports `Filters · N active` and keeps compact active-filter chips in view. Opening it exposes the Status, Labels, Search, Needs you, and Unread controls. The expanded label cloud has its own bounded scroll area, so it cannot displace issue rows.

Resolved asks leave the Inbox and open-ask badges, but their thread and Conversation card remain available with the actor and reason. `features/refs/MarkdownBody.tsx` renders every question, answer, comment, reply, and message through Proof's own parser, in every surface that shows one (Inbox, margin, Conversation, and the phone review sheet), not just the Conversation tab; its `inline` variant drops the wrapping block for option labels and clamped previews. The Conversation coalesces each ask lifecycle into one entry with its question, offered options, answer or resolution, and opened/completed timestamps. The margin keeps an anchored resolved ask visible as a closed decision without an answer form.

A document's approval (`features/doc/ApprovalChip.tsx`) is a human review pinned to a version: the header of an issue document, the issue's Spec header, and a project document page show a chip (`Awaiting approval`, `Approved v12`, `Approved v12 · changed since` when edited after approval, `Changes requested`) that opens the review history, plus `Approve` (`Approve v<latest>` when stale) and `Request changes` (reason required) controls that `POST /artifacts/{id}/reviews`; draft documents render no chip or approval controls. An agent's `dispatch_request_approval` opens an ask of `kind: "approval"`, which the Inbox card renders as `Approval requested` with the two fixed options and no Other row, requiring a `Reason` when `Request changes` is chosen; answering it is the same review. Approval is the exception path (Legion's design gate), so nothing about it is prominent.

Each open ask card (`features/inbox/`) keeps the full clarification exchange directly under its question (an inline reply list or collapsed reply-count disclosure) and uses one two-row composer for both outcomes. Urgency appears as a colored left border; blocking and high asks also have a small top-border notch, while every urgency remains in the article's accessible name. A session's tmux target stays compact in the metadata line as a click-to-copy control: it confirms a successful copy and, when neither clipboard path succeeds, tells the user to select the still-selectable target. A human chooses any fixed options and can type a free-text answer or note, then selects **Answer** or **Ask back**; question-shaped free text with no option chosen is offered as a clarification first, while answered asks retain their separate **Reply** composer. An action ask keeps its `Action` chip, compact opened age, and fixed `Done` / `Can't` options (`Can't` requires an explanation).

`AskCard` has `full` and `compact` variants. Inbox and Conversation use the full two-row composer. Margin and the responsive review sheet use the compact variant, where fixed options are touch-sized quick-action chips and a separate disclosure opens the free-text composer.

Each answer includes the nullable `edited_at` revision the human reviewed. On an `ASK_EDITED` response, the card reloads the latest question, keeps the draft text, clears its selected option, and requires explicit reconfirmation.

The Inbox partitions open asks by whose turn it is, preserving the server's priority order within `Waiting on you`, `Needs you`, and `Waiting on agents`; its `Blocked on you: N items, oldest 6h` banner finds the oldest waiting ask. No row appears in more than one partition. The sidebar Inbox entry and compact top bar report only `waitingOnYou` rows as `Needs you N`; each issue header applies that same predicate to its own open asks and shows `Waiting on you (N)` or `Waiting on agents (N)` immediately.

`web/src/components/` owns the shared status, label, approval, priority, empty, and loading primitives. Project lists and boards, Inbox, the margin, and Agents use the same icon-less empty state and card-shaped skeleton, so every loading or empty result preserves the surface's hierarchy.

## Margin threads

The document margin groups each issue-owned anchored root comment and all of its replies into one
flat thread card. A collapsed card shows a two-line root preview and reply summary; selecting it
expands the replies and an inline reply composer. An expanded card and its Proof mark
cross-highlight, and the card aligns to the mark. When a stored block anchor's mark is gone, its
card stays aligned to and highlights the `#b-<blockId>` block; a quote that spans top-level blocks
has no block anchor and retains existing orphan behavior. The block reference gutter opens the margin
filtered to one block. Project-document thread replies are handled by the margin-by-owner surface.

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
After the first paint, Dispatch warms that schema and headless Markdown engine so a long-lived tab
holds the lazy chunk through an SPA replacement. A stale Vite chunk reloads the page once per
session; if rendering still fails, `MarkdownBody` exposes its literal-text fallback with
`data-markdown-fallback`.
On focus no more than once a minute, the SPA checks the server health and fresh `index.html`; a
new entry chunk offers a dismissible **Reload** notice.

`ask` is the host-rendered decision type. A live document's open block decisions appear in one compact, cycling `#b-<blockId>` navigation link beneath the tab row; the in-document form and Inbox submit the same answer route, and an answered block is read-only. An ask block marked `invalid`, or one with a missing question or option label, renders its raw content as a malformed decision without answer controls until the block text is repaired. The editor library supplies its schema-aware Insert and Turn into block-menu entries; Dispatch passes the server schema rather than duplicating those commands.


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
