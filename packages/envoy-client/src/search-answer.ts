import {
  SEARCH_KIND_DEPTH,
  type SearchResponse,
  type SearchResult,
  snippetText,
} from "@legion/contracts";
import { dispatchDocumentRef } from "./dispatch-owner";

/** The "showing a-b of N" phrasing a paged tool result builds from its offset, row count, and total. */
export function pageSummaryText(offset: number, count: number, total: number): string {
  return `showing ${offset + 1}-${offset + count} of ${total}`;
}

function searchResultLine(result: SearchResult, baseUrl: string): string {
  const href = new URL(result.href, baseUrl).toString();
  const { owner } = result;
  if (owner.kind === "document") {
    const reference = dispatchDocumentRef(owner.project, owner.slug);
    return `${reference} [document] ${owner.name} - ${result.kind}: ${snippetText(result.snippet)} -> ${href}`;
  }
  const artifactName = result.artifact ? ` ${result.artifact.name}` : "";
  const label = `${owner.key} [${owner.status}] ${owner.title} - ${result.kind}${artifactName}`;
  return `${label}: ${snippetText(result.snippet)} -> ${href}`;
}

/**
 * The `dispatch_search` tool's text and details for one answer: every existing branch (a legacy
 * Dispatch that predates paging and omits `total`, the offset-past-end refusal, the cut line when
 * a kind's cap keeps some matches unreachable, and the next-offset line) unchanged, just moved out
 * of `dispatch-execute.ts`'s `case "dispatch_search"` so the wording lives in its own module, as
 * `ask-answer.ts` and `delivery.ts` already do for their own tools. `offset` is the offset the
 * caller requested (possibly undefined), used only for the legacy-Dispatch refusal message;
 * `search.offset` is the server's own answer.
 */
export function searchAnswer(
  search: SearchResponse,
  query: string,
  offset: number | undefined,
  configUrl: string
): { readonly text: string; readonly details: Record<string, unknown> } {
  const results = search.results;
  const count = results.length;
  const lines = results.map((result) => searchResultLine(result, configUrl));
  const noun = count === 1 ? "result" : "results";
  const noResults = `No results for "${query}".`;
  // A Dispatch from before search paging answers no total and serves its first page whatever
  // the offset, so a later page from it would silently repeat the first.
  if (typeof search.total !== "number") {
    if (offset !== undefined && offset > 0) {
      throw new Error(
        `Dispatch answered without a total: it predates search paging and ignored offset ${offset}, so this would be its first page again.`
      );
    }
    return {
      text:
        count === 0
          ? noResults
          : [`${count} ${noun} for "${query}" (${search.took_ms} ms)`, ...lines].join("\n"),
      details: { query, results },
    };
  }
  const { total, reachable, offset: pageOffset } = search;
  if (reachable === undefined || pageOffset === undefined) {
    // Every Dispatch that answers a total answers reachable and offset alongside it
    // (packages/contracts/src/dispatch-api.ts's SearchResponse); the type just can't say so.
    throw new Error("Dispatch answered a total without reachable or offset.");
  }
  const end = pageOffset + count;
  const cut =
    reachable < total
      ? `Each kind lists only its best ${SEARCH_KIND_DEPTH} matches, so ${reachable} of the ${total} can be paged to; narrow the query or name a project to reach the rest.`
      : undefined;
  const details = {
    query,
    results,
    total,
    reachable,
    offset: pageOffset,
    limit: search.limit,
  };
  if (count === 0) {
    return {
      text:
        total === 0
          ? noResults
          : [
              `No results for "${query}" at offset ${pageOffset}: it matches ${total}, and the pages reach the first ${reachable}.`,
              ...(cut === undefined ? [] : [cut]),
            ].join("\n"),
      details,
    };
  }
  const showing =
    pageOffset === 0 && count === total ? "" : `${pageSummaryText(pageOffset, count, total)}, `;
  return {
    text: [
      `${count} ${noun} for "${query}" (${showing}${search.took_ms} ms)`,
      ...(cut === undefined ? [] : [cut]),
      ...lines,
      ...(end < reachable ? [`Next page: offset ${end}.`] : []),
    ].join("\n"),
    details,
  };
}
