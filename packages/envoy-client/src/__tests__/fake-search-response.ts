import type {
  SearchResponse,
  SearchResult,
  SearchResultsPage,
  SearchResultsPageAbsentAs,
} from "@legion/contracts";

/** `fakeSearchResponse`'s paging override: all four of `SearchResultsPage`'s fields together, or
 * none of them (the same all-or-nothing shape `SearchResponse` itself enforces), never a subset —
 * `{ limit: 5 }` alone fails to compile. It does not forbid one field explicitly `undefined`
 * while the rest are numbers (`{ total: undefined, reachable: 5, limit: 5, offset: 0 }` still
 * compiles): without project-wide `exactOptionalPropertyTypes`, TypeScript always widens an
 * optional property to admit an explicit `undefined` regardless of its declared type, so `never`
 * closes out a concrete value but not that one. `fakeSearchResponse` below catches that residual
 * gap at runtime instead. */
type SearchResponseOverrides = { readonly took_ms?: number } & (
  | SearchResultsPageAbsentAs<never>
  | SearchResultsPage
);

const PAGING_FIELDS = ["total", "reachable", "limit", "offset"] as const;

/** A `dispatch_search` response body for one page of `hits`: paging fields default to a single
 * complete page (`total`/`reachable` equal to the hit count, `limit` 20, `offset` 0), overridable
 * for a test that exercises paging. Throws if an override leaves the merged response half-present
 * (some of `total`/`reachable`/`limit`/`offset` numbers, the rest explicitly `undefined`) — the
 * one invalid shape `SearchResponseOverrides`'s type can't reject on its own. */
export function fakeSearchResponse(
  hits: readonly SearchResult[],
  overrides: SearchResponseOverrides = {}
): SearchResponse {
  const merged = {
    results: hits as SearchResult[],
    total: hits.length,
    reachable: hits.length,
    limit: 20,
    offset: 0,
    took_ms: 0,
    ...overrides,
  };
  const undefinedFields = PAGING_FIELDS.filter((field) => merged[field] === undefined);
  if (undefinedFields.length > 0 && undefinedFields.length < PAGING_FIELDS.length) {
    throw new Error(
      `fakeSearchResponse: half-present paging override left ${undefinedFields.join(", ")} ` +
        "undefined while the rest answered numbers; override all four paging fields together or none."
    );
  }
  return merged as SearchResponse;
}
