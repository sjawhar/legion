import { SEARCH_RESULT_KINDS } from "@legion/contracts";
import type { SearchOwner, SearchResult, SearchResultKind } from "../../api/types";

/** `value` as a result kind, or `undefined` where this dashboard knows no such kind. Every
 *  surface that needs the kind itself - the palette, the related-items list - narrows here. */
export function searchResultKind(value: string): SearchResultKind | undefined {
  return SEARCH_RESULT_KINDS.find((known) => known === value);
}

export interface ResultGroup {
  readonly owner: SearchOwner;
  readonly results: SearchResult[];
}

/** Groups retain the rank order of their first server result. */
export function groupResults(results: readonly SearchResult[]): ResultGroup[] {
  const groups = new Map<string, ResultGroup>();

  for (const result of results) {
    const { owner } = result;
    // The owner is one flat record carrying the fields of whichever arm it is: an issue owner
    // names `key`, a document owner `artifact_id`.
    const ownerKey =
      owner.kind === "issue" ? `issue:${owner.key ?? ""}` : `document:${owner.artifact_id ?? ""}`;
    const group = groups.get(ownerKey);
    if (group === undefined) {
      groups.set(ownerKey, { owner, results: [result] });
    } else {
      group.results.push(result);
    }
  }

  return [...groups.values()];
}

/** Mirrors the server's first positive websearch token for document deep links. */
export function firstHighlightTerm(query: string): string | undefined {
  for (const token of query.trim().split(/\s+/u)) {
    if (token === "" || token.startsWith("-") || token.toLowerCase() === "or") {
      continue;
    }
    const term = token.replace(/^"+|"+$/gu, "");
    if (term !== "") {
      return term;
    }
  }
  return undefined;
}

export function stepActive(index: number, delta: 1 | -1, count: number): number {
  return count === 0 ? 0 : (index + delta + count) % count;
}

export function kindLabel(kind: SearchResultKind): "issue" | "doc" | "comment" | "ask" | "message" {
  return kind === "document" ? "doc" : kind;
}

export function optionId(result: SearchResult): string {
  return `search-option-${result.kind}-${result.id}`;
}
