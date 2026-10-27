/** What `two-builds.multiprocess.e2e.ts` walks: every route of the router in `web/src/app.tsx`,
 *  each with the page component it renders, and the lazily loaded pieces that are no route of
 *  their own. A page whose chunk no walked route loads is a chunk the walk never asks the other
 *  build's process for, so `e2e/__tests__/two-builds-routes.test.ts` holds this list to the
 *  router: every `lazy()` page and every `<Route>` of `app.tsx` must be here, and nothing else.
 *
 *  This module holds data only, so the bun test can import it without Playwright. */

/** One `<Route>` of `app.tsx`: its `path` as the router writes it, and the component it renders. */
export interface WalkRoute {
  readonly path: string;
  readonly page: string;
}

export const walkRoutes = [
  { page: "InboxPage", path: "/" },
  { page: "AnswersPage", path: "/answers" },
  { page: "RepliesPage", path: "/replies" },
  { page: "AgentsPage", path: "/agents" },
  { page: "BroadcastsPage", path: "/agents/broadcasts" },
  { page: "BroadcastPage", path: "/agents/broadcasts/:id" },
  { page: "AgentConversationPage", path: "/agents/:sessionId/live" },
  { page: "AgentArtifactPage", path: "/agents/:sessionId/artifacts/:slug" },
  { page: "IssuePage", path: "/issues/:key/*" },
  { page: "ProjectPage", path: "/projects/:key" },
  { page: "ProjectPage", path: "/projects/:key/architecture" },
  { page: "ProjectPage", path: "/projects/:key/issues" },
  { page: "ProjectPage", path: "/projects/:key/documents" },
  { page: "DocumentPage", path: "/projects/:key/documents/:slug" },
  { page: "CredentialRecordPage", path: "/credentials/:recordId" },
  { page: "MachineLoginPage", path: "/credentials/machine" },
  { page: "SettingsPage", path: "/settings" },
  { page: "DeliveryPage", path: "/delivery" },
  { page: "PipelinePage", path: "/pipeline" },
  { page: "NotFoundPage", path: "*" },
] as const satisfies readonly WalkRoute[];

export type WalkRoutePath = (typeof walkRoutes)[number]["path"];

/** The chunks a page loads only once it needs them, each named by what shows it has run. */
export const lazyPieces = {
  echarts: "the delivery timeline's chart, echarts (features/delivery/Timeline.tsx)",
  editor:
    "the document editor, @legion/proof-editor and its stylesheet (features/doc/editor.ts), " +
    "and the document's Yjs stack: yjs, @hocuspocus/provider, pending-edits and pending-sync " +
    "(features/doc/connection.ts)",
  "headless-markdown":
    "the headless Markdown renderer, @legion/proof-editor/headless, which every page warms at " +
    "boot (features/refs/markdown-engine.ts)",
} as const;

export type LazyPiece = keyof typeof lazyPieces;
