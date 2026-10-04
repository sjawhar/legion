import type { SearchResponse, SearchResult } from "@legion/contracts";

/** A `dispatch_search` response body for one page of `hits`: paging fields default to a single
 * complete page (`total`/`reachable` equal to the hit count, `limit` 20, `offset` 0), overridable
 * for a test that exercises paging. */
export function fakeSearchResponse(
  hits: readonly SearchResult[],
  overrides: Partial<Omit<SearchResponse, "results">> = {}
): SearchResponse {
  return {
    results: hits as SearchResult[],
    total: hits.length,
    reachable: hits.length,
    limit: 20,
    offset: 0,
    took_ms: 0,
    ...overrides,
  };
}
