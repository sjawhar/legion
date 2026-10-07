import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router-dom";

/** One `update` callback that merges a mutation into the live search params and writes it with
 *  `replace: true`, so repeated filter changes never pile up history entries. Shared by every
 *  URL-backed filter state (`useIssueFilters`, the delivery page's `useDeliveryUrlState`). */
export function useSearchParamsUpdate(): (mutate: (params: URLSearchParams) => void) => void {
  const [, setSearchParams] = useSearchParams();
  return useCallback(
    (mutate: (params: URLSearchParams) => void) => {
      setSearchParams(
        (current) => {
          const next = new URLSearchParams(current.toString());
          mutate(next);
          return next;
        },
        { replace: true }
      );
    },
    [setSearchParams]
  );
}

/** Every value of each repeatable `?<name>=` parameter in `keys`, memoized on the live
 *  `URLSearchParams` object's identity so two renders of the same URL return the exact same
 *  arrays — without this, `searchParams.getAll` mints a fresh array every render, and a consumer
 *  that keys a `useMemo` or a query off the result (`deliveryTimelineQuery`'s options) never sees
 *  two renders agree, recomputing or refetching on every render instead of only when the URL
 *  changes. `keys` is expected to be a module-level constant, since its own identity is a
 *  dependency of the memo. */
export function useRepeatableSearchParams<K extends string>(
  keys: readonly K[]
): Record<K, string[]> {
  const [searchParams] = useSearchParams();
  return useMemo(() => {
    const result = {} as Record<K, string[]>;
    for (const key of keys) result[key] = searchParams.getAll(key);
    return result;
  }, [searchParams, keys]);
}
