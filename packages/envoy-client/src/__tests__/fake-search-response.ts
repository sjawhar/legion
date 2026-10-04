import type { SearchResponse, SearchResult, SearchResultsPage } from "@legion/contracts";

/** `fakeSearchResponse`'s paging override: all four of `SearchResultsPage`'s fields together, or
 * none of them (the same all-or-nothing shape `SearchResponse` itself enforces) — never a subset,
 * and never one of them explicitly `undefined` while another is a number. */
type SearchResponseOverrides =
  | ({ readonly took_ms?: number } & { readonly [K in keyof SearchResultsPage]?: never })
  | ({ readonly took_ms?: number } & SearchResultsPage);

/** A `dispatch_search` response body for one page of `hits`: paging fields default to a single
 * complete page (`total`/`reachable` equal to the hit count, `limit` 20, `offset` 0), overridable
 * for a test that exercises paging. */
export function fakeSearchResponse(
  hits: readonly SearchResult[],
  overrides: SearchResponseOverrides = {}
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
