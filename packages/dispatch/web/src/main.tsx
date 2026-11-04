import { QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

import { whoAmIQuery } from "./api/queries";
import { DispatchQueryClient, queryRetry, queryRetryDelay } from "./api/query-client";
import { App } from "./app";
import { openDocumentEarly } from "./features/doc/open-document-early";
import {
  DeploymentResilience,
  installChunkFailureRecovery,
} from "./features/shell/DeploymentResilience";
import { ErrorBoundary } from "./features/shell/ErrorBoundary";
import "./styles.css";

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

// The sign-in check, asked for as the entry evaluates. Every page waits on it - the shell
// renders its skeleton until it answers (`app.tsx`'s `AuthGate`) - and it used to be asked for
// from that gate's own first commit, about 800 ms into React's boot on a throttled phone. Who
// may read what is untouched: the same check, the same answer, the same gate.
void queryClient.prefetchQuery(whoAmIQuery());

// The address this page was opened at, read before React renders anything: a document opens on
// its own text, and nothing about that read waits on the shell, the sign-in check or the issue
// page (`open-document-early.ts`, dispatch://INFRA-341 R11). A later address opens from the
// shell's own effect, and this one is not opened twice.
openDocumentEarly(queryClient, window.location.pathname, window.location.search);

const root = document.getElementById("root");
if (root === null) {
  throw new Error("Dispatch could not find its root element.");
}

installChunkFailureRecovery();

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <DeploymentResilience />
        <ErrorBoundary region="Dispatch">
          <App />
        </ErrorBoundary>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>
);
