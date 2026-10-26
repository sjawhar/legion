import type { QueryClient } from "@tanstack/react-query";

const WHOLE_CACHE_PROBE = ["whole-cache-refresh-probe"] as const;

/**
 * Counts whole-cache refreshes: a refresh that names no target marks every query the app holds
 * stale, and no event names this probe's key, so each time the probe goes stale is one of them.
 * The probe is made fresh again at once, ready for the next. Returns the count and a restore.
 */
export function countWholeCacheRefreshes(queryClient: QueryClient): {
  count: () => number;
  restore: () => void;
} {
  let refreshes = 0;
  queryClient.setQueryData(WHOLE_CACHE_PROBE, refreshes);
  const unsubscribe = queryClient.getQueryCache().subscribe((change) => {
    if (
      change.type === "updated" &&
      change.action.type === "invalidate" &&
      change.query.queryKey[0] === WHOLE_CACHE_PROBE[0]
    ) {
      refreshes += 1;
      queryClient.setQueryData(WHOLE_CACHE_PROBE, refreshes);
    }
  });
  return {
    count: () => refreshes,
    restore: () => {
      unsubscribe();
      queryClient.removeQueries({ exact: true, queryKey: WHOLE_CACHE_PROBE });
    },
  };
}

export function openStreamResponse(
  signal: AbortSignal | null | undefined,
  ...frames: string[]
): Response {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      const encoder = new TextEncoder();
      for (const frame of frames) {
        controller.enqueue(encoder.encode(frame));
      }
      // Never close on its own — this simulates a live, connected stream that stays
      // open until the client aborts it.
      signal?.addEventListener("abort", () => {
        controller.error(new DOMException("Aborted", "AbortError"));
      });
    },
  });
  return new Response(body, { status: 200 });
}
