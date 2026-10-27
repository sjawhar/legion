import {
  type AskAnswer,
  type AskResolution,
  SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE,
  SEARCH_KIND_DEPTH,
  type SearchAsk,
  type SearchResponse,
  type SearchResult,
  snippetText,
} from "@legion/contracts";
import { actorText } from "./actor-text";
import { dispatchDocumentRef } from "./dispatch-owner";

/** The "showing a-b of N" phrasing a paged tool result builds from its offset, row count, and total. */
export function pageSummaryText(offset: number, count: number, total: number): string {
  return `showing ${offset + 1}-${offset + count} of ${total}`;
}

function searchHitLine(result: SearchResult, baseUrl: string): string {
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

/** An answer's two lines under its hit: who gave it, when and the options chosen, then its text,
 *  every later line of which is indented four spaces so it stays under the hit. */
function answerLines(lead: string, answer: AskAnswer): string[] {
  const selected = answer.selected.length === 0 ? "none" : answer.selected.join(", ");
  const text =
    answer.text === null || answer.text === ""
      ? "none"
      : answer.text.split(/\r?\n/u).join("\n    ");
  return [
    `  ${lead} by ${answer.user} at ${answer.at} · options chosen: ${selected}`,
    `  Answer text: ${text}`,
  ];
}

function resolutionText(resolution: AskResolution): string {
  return `${resolution.kind} by ${actorText(resolution.actor)} at ${resolution.at}: ${resolution.reason}`;
}

/** Where an ask hit stands, as the lines under its hit; `undefined` is a Dispatch that predates
 *  reporting it. */
function askStateLines(ask: SearchAsk | undefined): string[] {
  if (ask === undefined) {
    return ["  Answer state not reported by this Dispatch; open the link to read the ask."];
  }
  switch (ask.state) {
    case "open":
      return ["  Open: no answer yet."];
    case "answered":
      return answerLines("Answered", ask.answer);
    case "resolved":
      return ask.answer === undefined
        ? [`  Closed with no answer: ${resolutionText(ask.resolution)}`]
        : [
            `  Closed after an answer: ${resolutionText(ask.resolution)}`,
            ...answerLines("Last answer", ask.answer),
          ];
  }
}

/** One hit as text: its line, then, for an ask, where the ask stands. */
function searchResultLine(result: SearchResult, baseUrl: string): string {
  const line = searchHitLine(result, baseUrl);
  return result.kind === "ask" ? [line, ...askStateLines(result.ask)].join("\n") : line;
}

/**
 * The `dispatch_search` tool's text and details for one answer: refuses a later page from a
 * legacy Dispatch that predates paging and always answers its first page regardless of offset,
 * names a kind's cap when it keeps some matches unreachable, and names the next offset when more
 * remain. `offset` is the offset the caller requested (possibly undefined), used only for the
 * legacy-Dispatch refusal message; `search.offset` is the server's own answer.
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
  // Meaning search is off for this request (no Bedrock credentials, a timed-out or failing query
  // embedding): the ranking below is keyword-only, and the caller is told so rather than
  // silently reading a hybrid ranking that did not run.
  const degraded =
    search.degraded === SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE
      ? "Searched by keyword only: meaning search was unavailable for this request."
      : undefined;
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
          ? [noResults, ...(degraded === undefined ? [] : [degraded])].join("\n")
          : [
              `${count} ${noun} for "${query}" (${search.took_ms} ms)`,
              ...(degraded === undefined ? [] : [degraded]),
              ...lines,
            ].join("\n"),
      details: { query, results, ...(degraded === undefined ? {} : { degraded: search.degraded }) },
    };
  }
  const { total, reachable, offset: pageOffset } = search;
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
    ...(degraded === undefined ? {} : { degraded: search.degraded }),
  };
  if (count === 0) {
    return {
      text:
        total === 0
          ? [noResults, ...(degraded === undefined ? [] : [degraded])].join("\n")
          : [
              `No results for "${query}" at offset ${pageOffset}: it matches ${total}, and the pages reach the first ${reachable}.`,
              ...(degraded === undefined ? [] : [degraded]),
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
      ...(degraded === undefined ? [] : [degraded]),
      ...(cut === undefined ? [] : [cut]),
      ...lines,
      ...(end < reachable ? [`Next page: offset ${end}.`] : []),
    ].join("\n"),
    details,
  };
}
