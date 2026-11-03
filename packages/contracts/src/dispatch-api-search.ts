import type { AskAnswer, AskResolution } from "./dispatch-api-asks";
import type { SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE } from "./dispatch-api-limits";

/**
 * Search and the related-item check: what `GET /api/v1/search` answers, how much of the corpus
 * has a current vector, and the items a write is told are like it.
 */

export type SearchResultKind = "issue" | "document" | "comment" | "ask" | "message";

export interface SearchArtifactRef {
  readonly slug: string;
  readonly name: string;
}

export type SearchOwner =
  | {
      readonly kind: "issue";
      readonly key: string;
      readonly title: string;
      readonly status: string;
    }
  | {
      readonly kind: "document";
      readonly project: string;
      readonly slug: string;
      readonly artifact_id: string;
      readonly name: string;
    };

/** Where an ask hit stands, read when the search ran. `answer` on a resolved ask is the last
 *  answer a person gave before it was closed (rows document settlement retracted while answered). */
export type SearchAsk =
  | { readonly state: "open" }
  | { readonly state: "answered"; readonly answer: AskAnswer }
  | { readonly state: "resolved"; readonly resolution: AskResolution; readonly answer?: AskAnswer };

export interface SearchResult {
  readonly kind: SearchResultKind;
  /** Who owns the hit: the issue (with its title and status) or the standalone project document. */
  readonly owner: SearchOwner;
  /**
   * Mirrors `owner` for issue-owned hits so agent clients built before the owner-only shape
   * (#1119) keep rendering; remove once no installed pi-legion-envoy / opencode-legion-envoy /
   * claude-envoy (or a legacy claude-envoy-bridge install) predates it. Clients built after #1119
   * read `owner`, never this.
   */
  readonly issue?: {
    readonly key: string;
    readonly title: string;
    readonly status: string;
  };
  readonly artifact?: SearchArtifactRef;
  readonly id: string;
  readonly snippet: string;
  /** The hit's fused score: 1/(60 + its position in its kind's ranked list). */
  readonly rank: number;
  readonly href: string;
  /** Where the ask stands: present on every `ask` hit from a Dispatch of this release, absent on
   *  every other kind and on any hit from an older Dispatch. */
  readonly ask?: SearchAsk;
}

/** The four paging fields a Dispatch that supports search paging always answers together. */
export interface SearchResultsPage {
  /** Every match of every kind. */
  readonly total: number;
  /**
   * How many of `total` the pages can return: each kind lists only its best `SEARCH_KIND_DEPTH`
   * (`dispatch-tools.ts`), so an offset at or past this returns no results.
   */
  readonly reachable: number;
  readonly limit: number;
  readonly offset: number;
}

/** `SearchResultsPage` with every field retyped to `V`, always optional: the shape of "none of
 * these fields" for a sentinel `V` (`undefined` for the wire type below, `never` for
 * `fake-search-response.ts`'s stricter test-only override, which forbids supplying a concrete
 * value without forbidding an explicit `undefined` — TypeScript widens every optional property to
 * admit one regardless of its declared type unless the whole project turns on
 * `exactOptionalPropertyTypes`). */
export type SearchResultsPageAbsentAs<V> = { readonly [K in keyof SearchResultsPage]?: V };

/** One page of `GET /api/v1/search`'s fused order, cut at `offset` and `limit`. `total`,
 * `reachable`, `limit`, and `offset` answer together or not at all: a Dispatch that predates
 * search paging omits all four and always answers its first page regardless of any offset
 * requested; one that supports it always answers all four together (`SearchResultsPage`). */
export type SearchResponse = {
  readonly results: SearchResult[];
  readonly took_ms: number;
  /**
   * Why this search fell back to keyword-only ranking - today only
   * `SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE` (LEGION-549: no embedder configured, the query's own
   * embedding timed out, or the model service answered an error) - absent when meaning search
   * ran normally.
   */
  readonly degraded?: typeof SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE;
} & (SearchResultsPage | SearchResultsPageAbsentAs<undefined>);

/** One kind's (or every kind's) counts in `SearchCoverage`. `items` is every searchable item;
 * `current` those with a vector from the configured embedder's identity at the item's current
 * text, `failed` those whose row is dead and not current, and `lacking` the rest
 * (`items - current - failed`). `oldest_lacking_seconds` is how long the longest-waiting lacking
 * item has waited for its vector, from when its row last became pending or when the item was
 * written, whichever is later; null when nothing lacks. Mirrors `model.SearchCoverageCounts`. */
export interface SearchCoverageCounts {
  readonly items: number;
  readonly current: number;
  readonly lacking: number;
  readonly failed: number;
  readonly oldest_lacking_seconds: number | null;
}

/** `SearchCoverage`'s counts for each kind meaning search ranks. Mirrors
 * `model.SearchCoverageKinds`. */
export interface SearchCoverageKinds {
  readonly issue: SearchCoverageCounts;
  readonly document: SearchCoverageCounts;
  readonly comment: SearchCoverageCounts;
  readonly ask: SearchCoverageCounts;
  readonly message: SearchCoverageCounts;
}

/** `GET /api/v1/search/coverage`: how much of what meaning search ranks has a current vector from
 * the configured embedder (INFRA-393). `identity` is the value `embeddings.model` records and
 * search compares, `place` and `model` are read from it; all three are null when no embedder is
 * configured, and then nothing is current. Mirrors `model.SearchCoverage`. */
export interface SearchCoverage {
  readonly identity: string | null;
  readonly place: "bedrock" | "azure" | null;
  readonly model: string | null;
  readonly total: SearchCoverageCounts;
  readonly kinds: SearchCoverageKinds;
}

export interface DuplicateCandidate {
  readonly key: string;
  readonly title: string;
  readonly status: string;
  readonly snippet: string;
  readonly shared_terms: number;
  readonly href: string;
}

/**
 * One item the related-item check (LEGION-550) judges like what was just filed: a
 * search hit over the same project. `ask` is where an ask item stands, absent from an older
 * Dispatch and when the check could not read the asks' states (no item then carries `ask`, and
 * there is no `decision`). `answered_by`/`answered_at` are set only when this is
 * `Suggestions.decision` — the "past decision" case, where the agent needs who answered and when,
 * not just a link — and repeat `ask.answer`'s user and time for clients built before `ask`.
 */
export interface WriteSuggestion {
  readonly kind: SearchResultKind;
  readonly owner: SearchOwner;
  readonly artifact?: SearchArtifactRef;
  readonly id: string;
  readonly snippet: string;
  readonly href: string;
  readonly ask?: SearchAsk;
  readonly answered_by?: string;
  readonly answered_at?: string;
}

/** `Suggestions.outcome`: the check listed items, searched and found none, or did not search to
 *  the end (`missing` says why). */
export type RelatedCheckOutcome = "found" | "none" | "not_made";

/** `Suggestions.search`: how the check searched; keywords only when meaning search could not run
 *  (`search_note` says why) or the caller asked for it. */
export type RelatedCheckSearch = "meaning_and_keywords" | "keywords_only";

/**
 * The related-item check's result (LEGION-550) on a newly created issue or ask, on
 * each new decision block of a document edit, and as the whole answer of the no-write related
 * route: the three items most like it (`related`, one per owner, always an array) and, when an
 * answered ask already settles the same question, that decision (`decision`). The check never
 * refuses the write; it waits at most five seconds.
 *
 * A Dispatch predating the check's outcome sends only `related`, `decision` and `missing`; every other
 * field is absent there. From that release on, `outcome` is always set, `search` is set unless the
 * check was not made, and `missing` is set, to the reason, exactly when `outcome` is `not_made`.
 * `search_query` is two or three of the words the keyword search used, spelled as the text spells
 * them, for a reader to run `dispatch search` with. `title_matches` is the issues whose titles
 * near-duplicate a new issue's when the create was forced past that refusal. Mirrors
 * `model.Suggestions`.
 */
export interface Suggestions {
  readonly outcome?: RelatedCheckOutcome;
  readonly search?: RelatedCheckSearch;
  readonly search_note?: string;
  readonly search_query?: string;
  readonly related: WriteSuggestion[];
  readonly decision?: WriteSuggestion;
  readonly missing?: string;
  readonly title_matches?: DuplicateCandidate[];
}

/** One new decision block's related-item check in a document edit's advice
 *  (`WriteAdvice.decision_block_checks`): the block's id, its question, and the check's result. */
export interface DecisionBlockCheck {
  readonly block_id: string;
  readonly question: string;
  readonly suggestions: Suggestions;
}

/**
 * The body of `POST /api/v1/projects/{key}/related` (and the query of its `GET` form, which caps
 * title and text together at `SEARCH_QUERY_MAX`): the related-item check over a title and a text
 * without writing anything. `exclude_issue` leaves that issue's own items out of the results, as a
 * check on that issue's own write does; `keywords_only` skips meaning search. A blank title and
 * text answer `not_made`. The answer is `Suggestions`.
 */
export interface RelatedCheckInput {
  readonly title?: string;
  readonly text?: string;
  readonly exclude_issue?: string;
  readonly keywords_only?: boolean;
}

/** One millisecond column of `RelatedChecksReport` over the checks that hold a value in it
 *  (`count`): discrete 50th and 95th percentiles, the largest, and how many took more than 300 ms.
 *  `p50`, `p95` and `max` are null when `count` is 0. Mirrors `model.RelatedCheckTiming`. */
export interface RelatedCheckTiming {
  readonly count: number;
  readonly p50: number | null;
  readonly p95: number | null;
  readonly max: number | null;
  readonly over_300ms: number;
}

/**
 * `GET /api/v1/related-checks?project=&from=&to=&source_kind=`: how the related-item checks of
 * writes recorded within one window went. `project`, `source_kind`, `from` and `to` echo the
 * filters, each null when not given (`from` inclusive, `to` exclusive). Each `by_*` map holds only
 * the values seen; `by_search` counts a check not made under `not_made`, and `by_reason` leaves
 * out checks with no reason. `completed` is the checks that searched to the end (found or none),
 * `skipped_for_time` those not made because the search ran out of time. Mirrors
 * `model.RelatedChecksReport`.
 */
export interface RelatedChecksReport {
  readonly project: string | null;
  readonly source_kind: string | null;
  readonly from: string | null;
  readonly to: string | null;
  readonly total: number;
  readonly by_source_kind: Readonly<Record<string, number>>;
  readonly by_outcome: Readonly<Record<string, number>>;
  readonly by_search: Readonly<Record<string, number>>;
  readonly by_reason: Readonly<Record<string, number>>;
  readonly completed: number;
  readonly skipped_for_time: number;
  readonly keyword_ms: RelatedCheckTiming;
  readonly total_ms: RelatedCheckTiming;
}
