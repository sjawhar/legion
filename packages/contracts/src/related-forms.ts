import type { Suggestions, WriteSuggestion } from "./dispatch-api";

/**
 * Which of the related-item check's three forms a `Suggestions` holds, judged once for every
 * reader of it. The CLI's lines (`packages/envoy-client/src/related-check.ts`) and the
 * dashboard's section (`packages/dispatch/web/src/features/issue/RelatedItems.tsx`) both read
 * this union and only word it; neither judges the result itself, so a check that never ran cannot
 * read as one that found nothing on one surface and not the other.
 *
 * - `not_made`: no check to trust, `reason` says why, and `searchQuery` is the server's
 *   `search_query` exactly as it sent it (untrimmed, possibly undefined), so each caller resolves
 *   the words to search by in its own words.
 * - `items`: the items the check found, with `mode` naming how it searched when the server said.
 * - `nothing`: the check searched, `mode` naming how, and found nothing.
 */
export type RelatedCheckForm =
  | {
      readonly form: "not_made";
      readonly reason: string;
      readonly searchQuery: string | undefined;
    }
  | {
      readonly form: "items";
      readonly mode: string | undefined;
      readonly related: readonly WriteSuggestion[];
    }
  | { readonly form: "nothing"; readonly mode: string | undefined };

/**
 * The form a check result holds. The branch order is the contract, not an implementation detail:
 * a result carrying both items and a not-made marker is not made, and a result with no `outcome`
 * is judged by what it carries, so a Dispatch predating the outcome field still maps onto a form.
 *
 * 1. `outcome` is `not_made`, or absent with a `missing` reason → not made, for that reason.
 * 2. `related` is not empty → the items.
 * 3. `outcome` is absent → not made: the server did not say whether it searched.
 * 4. `outcome` is `found` or `none` → the check searched and found nothing.
 * 5. anything else → not made: an outcome this client does not know.
 */
export function relatedCheckFormOf(suggestions: Suggestions): RelatedCheckForm {
  const { outcome, missing } = suggestions;
  const searchQuery = suggestions.search_query;
  if (outcome === "not_made" || (outcome === undefined && missing !== undefined)) {
    return { form: "not_made", reason: missing ?? "the server gave no reason", searchQuery };
  }
  const mode = relatedCheckModeWords(suggestions);
  if (suggestions.related.length > 0) {
    return { form: "items", mode, related: suggestions.related };
  }
  if (outcome === undefined) {
    return {
      form: "not_made",
      reason: "the server did not say whether it searched",
      searchQuery,
    };
  }
  if (outcome === "found" || outcome === "none") {
    return { form: "nothing", mode };
  }
  return {
    form: "not_made",
    reason: `the server answered an outcome this client does not know (${String(outcome)})`,
    searchQuery,
  };
}

/** How the check searched, as the forms word it after "searched" or "searched by"; undefined when
 *  the server named no mode, which is what a Dispatch predating the forms sends. */
export function relatedCheckModeWords(suggestions: Suggestions): string | undefined {
  const { search, search_note: note } = suggestions;
  if (search === undefined) return undefined;
  if (search === "meaning_and_keywords") return "meaning and keywords";
  const words = search === "keywords_only" ? "keywords only" : String(search);
  return note === undefined || note === "" ? words : `${words}: ${note}`;
}

/**
 * The words to search a not-made check by hand with, taken from the text the check read: its
 * first three words of four or more letters, joined by spaces, empty when it holds no such word.
 * Short words are mostly ones every item holds. Each caller decides what to say when it is empty,
 * and what to prefer over it (the server's `search_query`, when it sent one).
 */
export function relatedCheckSearchWords(text: string): string {
  return (text.match(/[\p{L}\p{N}_]+/gu) ?? [])
    .filter((word) => (word.match(/\p{L}/gu) ?? []).length >= 4)
    .slice(0, 3)
    .join(" ");
}
