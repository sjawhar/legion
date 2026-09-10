import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

import { isRetryableQueryError } from "./api/client";
import { App } from "./app";
import "./styles.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // An auth outcome (401/403) is definitive — everything else (a dropped connection,
      // a 5xx) is worth a couple of quick retries before we show an error.
      retry: (failureCount, error) => failureCount < 2 && isRetryableQueryError(error),
      retryDelay: 500,
      // The event stream is the source of freshness; a stale query still gets
      // invalidated the moment a relevant event arrives. This just avoids an
      // unconditional refetch-on-mount for data the stream keeps current.
      refetchOnWindowFocus: true,
      staleTime: 30_000,
    },
  },
});

const root = document.getElementById("root");
if (root === null) {
  throw new Error("Dispatch could not find its root element.");
}

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>
);
