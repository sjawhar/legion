import { describe, expect, test } from "bun:test";
import {
  SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE,
  type SearchResponse,
  type SearchResult,
} from "@legion/contracts";
import { searchAnswer } from "../search-answer";
import { fakeSearchResponse } from "./fake-search-response";

const hit: SearchResult = {
  kind: "comment",
  owner: { kind: "issue", key: "LEGION-2", title: "Astrolabe", status: "triage" },
  id: "comment-2",
  snippet: "Comment about <mark>astrolabe</mark>",
  rank: 0.5,
  href: "/issues/LEGION-2/spec#comment-2",
};

/** `fakeSearchResponse`'s overrides type excludes `degraded` (it is not a paging field), so a
 * degraded fixture is built by spreading it on afterward - `SearchResponse.degraded` is optional,
 * so this is still a valid `SearchResponse`. */
function degradedResponse(
  results: readonly SearchResult[],
  overrides: Parameters<typeof fakeSearchResponse>[1] = {}
): SearchResponse {
  return {
    ...fakeSearchResponse(results, overrides),
    degraded: SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE,
  };
}

describe("searchAnswer: degraded wording", () => {
  // LEGION-549's acceptance criterion ("with the embedder down, search answers with keyword
  // results and says it did") is a server-side behavior proven in Go
  // (TestSearchAnswersKeywordOnlyWhenTheEmbedderFails); this is the client-side half - that the
  // tool's own text and details surface the degraded flag the server sends, which was previously
  // untested at this layer (round-1/round-2 review, carried open through round 3).

  test("names the degraded reason in the text and carries it in details, with results", () => {
    const search = degradedResponse([hit], { took_ms: 9 });
    const { text, details } = searchAnswer(search, "astrolabe", undefined, "http://dispatch.test");

    expect(text).toContain(
      "Searched by keyword only: meaning search was unavailable for this request."
    );
    // The degraded line follows the count line and precedes the first result line.
    expect(text.split("\n")).toEqual([
      '1 result for "astrolabe" (9 ms)',
      "Searched by keyword only: meaning search was unavailable for this request.",
      "LEGION-2 [triage] Astrolabe - comment: Comment about **astrolabe** -> http://dispatch.test/issues/LEGION-2/spec#comment-2",
    ]);
    expect(details.degraded).toBe(SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE);
  });

  test("names the degraded reason even when the fallback finds nothing", () => {
    const search = degradedResponse([]);
    const { text, details } = searchAnswer(
      search,
      "quarterly budget notes",
      undefined,
      "http://dispatch.test"
    );

    expect(text).toBe(
      [
        'No results for "quarterly budget notes".',
        "Searched by keyword only: meaning search was unavailable for this request.",
      ].join("\n")
    );
    expect(details.degraded).toBe(SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE);
  });

  test("names the degraded reason on a legacy no-paging response too", () => {
    // A Dispatch that predates search paging still answers `degraded`: this is the response shape
    // search-answer.ts's `typeof search.total !== "number"` branch handles, whose own degraded
    // line was also never exercised by a test before this file.
    const search: SearchResponse = {
      results: [hit],
      took_ms: 5,
      degraded: SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE,
    };
    const { text, details } = searchAnswer(search, "astrolabe", undefined, "http://dispatch.test");

    expect(text.split("\n")[0]).toBe('1 result for "astrolabe" (5 ms)');
    expect(text).toContain(
      "Searched by keyword only: meaning search was unavailable for this request."
    );
    expect(details.degraded).toBe(SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE);
  });

  test("omits the degraded line and the details field entirely when meaning search ran normally", () => {
    const search = fakeSearchResponse([hit], { took_ms: 9 });
    const { text, details } = searchAnswer(search, "astrolabe", undefined, "http://dispatch.test");

    expect(text).not.toContain("keyword only");
    expect(details).not.toHaveProperty("degraded");
  });
});
