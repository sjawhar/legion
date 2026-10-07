import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { useCallback } from "react";
import { useSearchParams } from "react-router-dom";

/**
 * A free-text `?<name>=` parameter: read live, written with `replace` so filtering never piles up
 * history entries, and capped at `SEARCH_QUERY_MAX` characters, because the page's address is
 * where it lives and the load balancer refuses a URL past 16 K on reload or a shared link. The cut
 * lands on a code point: a trailing lone high surrogate is dropped rather than split, so a value
 * longer than the cap - typed, or arriving on a link - never leaves half an emoji in the address.
 * `useIssueFilters`'s `?q=` and the Agents page's `?q=` both go through this one hook.
 */
export function useCappedSearchParam(name: string): readonly [string, (next: string) => void] {
  const [searchParams, setSearchParams] = useSearchParams();
  const value = searchParams.get(name) ?? "";
  const setValue = useCallback(
    (next: string) => {
      setSearchParams(
        (current) => {
          const params = new URLSearchParams(current.toString());
          if (next === "") {
            params.delete(name);
          } else {
            const capped = next.slice(0, SEARCH_QUERY_MAX);
            params.set(name, /[\uD800-\uDBFF]$/.test(capped) ? capped.slice(0, -1) : capped);
          }
          return params;
        },
        { replace: true }
      );
    },
    [name, setSearchParams]
  );
  return [value, setValue] as const;
}
