import type { SearchIssueRef, SearchResult, SearchResultKind } from "../../api/types";

export interface ResultGroup {
  readonly issue: SearchIssueRef;
  readonly results: SearchResult[];
}

/** Groups retain the rank order of their first server result. */
export function groupResults(results: readonly SearchResult[]): ResultGroup[] {
  const groups = new Map<string, ResultGroup>();

  for (const result of results) {
    const group = groups.get(result.issue.key);
    if (group === undefined) {
      groups.set(result.issue.key, { issue: result.issue, results: [result] });
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
