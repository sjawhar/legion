import type { QueryClient } from "@tanstack/react-query";

/**
 * Counts whole-cache refreshes: `refreshQueries` is the only caller that invalidates with no key,
 * so an invalidation naming none is exactly one of them. Returns the count and a restore.
 */
export function countWholeCacheRefreshes(queryClient: QueryClient): {
  count: () => number;
  restore: () => void;
} {
  const invalidate = queryClient.invalidateQueries.bind(queryClient);
  let refreshes = 0;
  queryClient.invalidateQueries = ((filters?: { queryKey?: readonly unknown[] }) => {
    if (filters?.queryKey === undefined) {
      refreshes += 1;
    }
    return invalidate(filters);
  }) as typeof queryClient.invalidateQueries;
  return {
    count: () => refreshes,
    restore: () => {
      queryClient.invalidateQueries = invalidate;
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
