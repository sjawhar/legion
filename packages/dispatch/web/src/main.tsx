import { QueryClientProvider } from "@tanstack/react-query";
import { lazy, StrictMode, Suspense } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

import { DispatchQueryClient, queryRetry, queryRetryDelay } from "./api/query-client";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { ShellSkeleton } from "./components/ShellSkeleton";
import { rememberEarlyKeys } from "./early-keys";
import "./styles.css";

/**
 * The entry chunk: React, react-dom, the router, the query client and the shell's frame, and
 * nothing of this app's own features.
 *
 * The browser downloads this chunk, compiles it and evaluates every static import of it in one
 * main-thread task before anything of the page's own runs - 501 to 655 ms on a phone at 4x CPU
 * throttling while the app's own source was in it, over the 500 ms a reader feels as a freeze
 * (dispatch://INFRA-341, R11). A static module graph evaluates in one task whatever it holds,
 * so the only way to make that task smaller is to leave the app out of it: the app and the
 * page's first reads are asked for with `import()` below and evaluate in tasks of their own,
 * and `web/src/entry-chunk-contents.ts` holds this chunk to that.
 *
 * It costs no request. Both chunks are written into `index.html` for the page to preload
 * beside this one (`DOCUMENT_ADDRESS_MODULES` and `documentAddressChunks` in
 * `web/vite.config.ts`), from every address rather than only the two that open a document, so
 * their bytes are in hand by the time this task ends. And it costs the reader no frame: the
 * `Suspense` below shows the same skeleton `AuthGate` shows while the sign-in check is out.
 */

// Never two requests in flight for one read: an invalidation during a fetch fetches again once it
// settles (`api/query-client.ts`).
const queryClient = new DispatchQueryClient({
  defaultOptions: {
    queries: {
      // Two quick retries for a transient failure, none for a 401, and the sign-in check's
      // EVENT_HEAD_UNAVAILABLE for as long as it lasts (`queryRetry`), each after the wait a
      // server refusing excess load asks for (`Retry-After`, `queryRetryDelay`), or 500 ms.
      retry: queryRetry,
      retryDelay: queryRetryDelay,
      // The live socket's event feed is the source of freshness; a stale query still gets
      // invalidated the moment a relevant event arrives. This just avoids an unconditional
      // refetch-on-mount for data the feed keeps current.
      refetchOnWindowFocus: true,
      staleTime: 30_000,
    },
  },
});

/**
 * What the page does before it renders: the chunk-failure recovery, the sign-in check and the
 * address's own document (`features/shell/boot.ts`), in the first task after this one.
 *
 * Both chunks are asked for here, in the same task, rather than the app's waiting on this one:
 * each read this makes is keyed exactly as the app's own (`whoAmIQuery`, and
 * `open-document-early.ts`, which opens one address once), so a read the app reaches first is
 * the same read, and holding the app back until this had run only put this module's own
 * evaluation task in front of the reader's first screen. A boot that fails to load is not a
 * page that fails to show: the app renders, and asks for each read itself as it always did.
 */
// Hold a shortcut the reader presses at the skeleton, from this task, so the app's keymap can
// answer it when it binds (`early-keys.ts`).
rememberEarlyKeys();

void import("./features/shell/boot").then(
  ({ bootThePage }) => bootThePage(queryClient, window.location.pathname, window.location.search),
  () => undefined
);

const App = lazy(() => import("./app").then(({ App }) => ({ default: App })));

/** The long-lived tab's Markdown engine and the reload a new deployment offers. It draws
 *  nothing until there is something to offer, so it waits for its own chunk under a `null`
 *  fallback, outside the app's error boundary exactly as it was outside it in this module. */
const DeploymentResilience = lazy(() =>
  import("./features/shell/DeploymentResilience").then((module) => ({
    default: module.DeploymentResilience,
  }))
);

const root = document.getElementById("root");
if (root === null) {
  throw new Error("Dispatch could not find its root element.");
}

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <Suspense fallback={null}>
          <DeploymentResilience />
        </Suspense>
        <ErrorBoundary region="Dispatch">
          <Suspense fallback={<ShellSkeleton />}>
            <App />
          </Suspense>
        </ErrorBoundary>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>
);
