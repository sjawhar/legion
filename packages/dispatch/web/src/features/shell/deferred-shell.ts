import { lazy } from "react";

/**
 * The shell's parts that a document route does not draw first, each in a chunk of its own.
 *
 * A reader who opens `/issues/<KEY>/artifacts/<slug>` waits for the entry chunk to download,
 * compile and evaluate before anything of theirs runs - one main-thread task of about a second
 * on a phone at 4x CPU throttling (dispatch://INFRA-341, R11). None of the five below is on the
 * screen when the document's first block is, so none of them belongs in that task: the Inbox
 * drawer and the create-issue dialog open only when the reader asks for them, the agent registry
 * poller renders nothing at all, the margin is a column beside the document, and the reference
 * preview is a card that only a hover or a keyboard focus opens.
 *
 * Each still appears exactly when it appears today. The shell wraps each in the `Suspense` shape
 * `InboxList` and the `Routes` already use (`app.tsx`), with a `null` fallback: its chunk is
 * requested the first render that asks for it and the part fills in under the frame the shell
 * draws itself, as the Inbox list does while its own read is out.
 *
 * `app.tsx` declares no `lazy()` of its own beyond its routes, which `e2e/two-builds-routes.ts`
 * holds to the router, so these live here instead - as `InboxList` does beside the Inbox.
 */

/** The Inbox as a peek over whatever page is open; mounted by the shell while it is open. */
export const InboxDrawer = lazy(() =>
  import("../inbox/InboxDrawer").then((module) => ({ default: module.InboxDrawer }))
);

/** The create-issue dialog, mounted by the shell while it is open. */
export const CreateIssueDialog = lazy(() =>
  import("../issue/CreateIssueDialog").then((module) => ({ default: module.CreateIssueDialog }))
);

/** The margin beside a document surface; it renders nothing on a route that has none. */
export const Margin = lazy(() =>
  import("../margin/Margin").then((module) => ({ default: module.Margin }))
);

/** The reference hover card's host. Only the card is here: the attribute that makes a link a
 *  trigger, and the close every surface may call, are `features/refs/ref-preview-state.ts`. */
export const RefPreviewHost = lazy(() =>
  import("../refs/RefPreview").then((module) => ({ default: module.RefPreviewHost }))
);

/** The one owner of the agent registry's refresh cadence. It renders nothing, and nothing on the
 *  page waits for it: a reader it would have named is named from the cache until it lands. */
export const AgentRegistryPoller = lazy(() =>
  import("../refs/agent-registry").then((module) => ({ default: module.AgentRegistryPoller }))
);
