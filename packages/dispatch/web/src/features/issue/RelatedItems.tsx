import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useState } from "react";

import { api } from "../../api/client";
import type { Suggestions, WriteSuggestion } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  badgeLow,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { statusText } from "../project/board-model";
import { Timestamp } from "../refs/Timestamp";
import { AskHitStatus } from "../search/AskHitStatus";
import { kindLabel } from "../search/search-model";
import { RELATED_DEBOUNCE_MS } from "./related-timing";

/** Shorter than this, every item in the project would match, so nothing is asked. */
const RELATED_TITLE_MIN = 2;

/**
 * The check's answer for the title in the box, and whether that answer is still out. The query
 * key holds the title it answers, so react-query aborts a superseded check's request (its
 * `signal`) and an answer to an older title can never be stored under the key this title makes.
 */
function useRelatedItems(project: string, title: string) {
  const trimmed = title.trim();
  const [debounced, setDebounced] = useState("");
  useEffect(() => {
    const timeout = window.setTimeout(() => setDebounced(trimmed), RELATED_DEBOUNCE_MS);
    return () => window.clearTimeout(timeout);
  }, [trimmed]);
  const enabled = project !== "" && debounced.length >= RELATED_TITLE_MIN;
  // What is shown follows the same floor over the title in the box rather than the debounced
  // copy, so the section empties the moment the reader clears the box and nothing restates the
  // rule outside this hook.
  const shown = project !== "" && trimmed.length >= RELATED_TITLE_MIN;
  const query = useQuery({
    enabled,
    queryFn: ({ signal }) => api.relatedItems(project, debounced, { signal }),
    queryKey: ["related", project, debounced],
  });
  // The answer on screen is always the answer to the title in the box: while the debounced copy
  // lags the box, what the query holds answers a title the reader has already left.
  return { query, shown, trimmed, waiting: debounced !== trimmed };
}

/**
 * The related-item check over the title being typed, shown beside the draft so a duplicate is
 * seen before it is filed. The check's three forms are worded as the CLI words them
 * (`packages/envoy-client/src/related-check.ts`), so a check that never ran cannot be read as
 * one that found nothing. The section is always in the tree and polite, so each new answer is
 * announced rather than silently replacing the one a screen reader just read.
 */
export function RelatedItems({ project, title }: { project: string; title: string }): ReactNode {
  const { query, shown, trimmed, waiting } = useRelatedItems(project, title);
  let body: ReactNode;
  if (!shown) {
    body = null;
  } else if (!waiting && query.isError) {
    body = (
      <QueryError
        message="Could not check for related items."
        onRetry={() => void query.refetch()}
        retrying={query.isFetching}
      />
    );
  } else if (waiting || query.data === undefined) {
    body = <p className={textMutedOnSurface}>Checking for related items…</p>;
  } else {
    body = <RelatedForm suggestions={query.data} title={trimmed} />;
  }
  return (
    <section aria-label="Related items" aria-live="polite" className="space-y-2 text-sm">
      {body}
    </section>
  );
}

/**
 * Exactly one of the check's three forms, then the past decision when the check found one.
 * `title_matches` is not shown: this route writes nothing, so no title overlap was forced past.
 */
function RelatedForm({
  suggestions,
  title,
}: {
  suggestions: Suggestions;
  title: string;
}): ReactNode {
  const { decision, missing, outcome } = suggestions;
  const related = suggestions.related ?? [];
  const mode = searchModeWords(suggestions);
  const notMade = (reason: string): ReactNode => (
    <p className={textMutedOnSurface}>{notMadeText(reason, title, suggestions.search_query)}</p>
  );
  let form: ReactNode;
  if (outcome === "not_made" || (outcome === undefined && missing !== undefined)) {
    // The CLI tests this first (`relatedCheckForm`), so an answer carrying both items and a
    // not-made marker reads the same on both surfaces.
    form = notMade(missing ?? "the server gave no reason");
  } else if (related.length > 0) {
    form = (
      <>
        <p className={`font-medium ${textSecondaryOnSurface}`}>
          {/* A server predating the forms names no search mode, so the heading names none. */}
          {mode === undefined ? "Related items:" : `Related items (searched by ${mode}):`}
        </p>
        <ul className="space-y-2">
          {related.map((item) => (
            <RelatedItemRow item={item} key={`${item.kind}:${item.id}`} withAsk={true} />
          ))}
        </ul>
      </>
    );
  } else if (outcome === undefined) {
    form = notMade("the server did not say whether it searched");
  } else if (outcome === "found" || outcome === "none") {
    form = (
      <p className={textMutedOnSurface}>
        {mode === undefined
          ? "Related-item check searched and found nothing."
          : `Related-item check searched (${mode}) and found nothing.`}
      </p>
    );
  } else {
    form = notMade(`the server answered an outcome this client does not know (${String(outcome)})`);
  }
  return (
    <>
      {form}
      {decision === undefined ? null : (
        <div className="space-y-1">
          <p className={`font-medium ${textSecondaryOnSurface}`}>
            A past decision may already answer this:
          </p>
          <ul>
            {/* One item per owner can collapse the decision's ask out of the related items, so
                where its ask stands is shown below, once, under who answered it. */}
            <RelatedItemRow item={decision} withAsk={false} />
          </ul>
          <p className={`text-xs ${textMutedOnSurface}`}>
            Answered by {decision.answered_by}
            {decision.answered_at === undefined ? null : (
              <>
                {" · "}
                <Timestamp at={decision.answered_at} />
              </>
            )}
          </p>
          {decision.ask === undefined ? null : <AskHitStatus ask={decision.ask} muted={false} />}
        </div>
      )}
    </>
  );
}

/**
 * One item: its kind, where it stands, and a link whose whole text is the owner's label, so a
 * screen reader's link list names the item and nothing else. It opens in a new tab, since the
 * draft in the dialog would be lost by navigating away from it.
 */
function RelatedItemRow({ item, withAsk }: { item: WriteSuggestion; withAsk: boolean }): ReactNode {
  const { owner } = item;
  return (
    <li className="min-w-0">
      <div
        className={`flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5 text-xs ${textMutedOnSurface}`}
      >
        <span className={`rounded-full px-2 py-0.5 font-medium ${badgeLow.bg} ${badgeLow.text}`}>
          {kindLabel(item.kind)}
        </span>
        <span className="min-w-0 [overflow-wrap:anywhere]">
          {owner.kind === "issue" ? statusText(owner.status) : `${owner.project}/${owner.slug}`}
        </span>
      </div>
      <a
        className={`block min-w-0 [overflow-wrap:anywhere] ${linkText} ${linkHoverText}`}
        href={item.href}
        rel="noreferrer"
        target="_blank"
      >
        {owner.kind === "issue" ? `${owner.key} ${owner.title}` : owner.name}
      </a>
      {withAsk && item.kind === "ask" && item.ask !== undefined ? (
        <AskHitStatus ask={item.ask} muted={false} />
      ) : null}
    </li>
  );
}

/** How the check searched, as the forms word it after "searched" or "searched by". */
function searchModeWords(suggestions: Suggestions): string | undefined {
  const { search, search_note: note } = suggestions;
  if (search === undefined) return undefined;
  if (search === "meaning_and_keywords") return "meaning and keywords";
  const words = search === "keywords_only" ? "keywords only" : String(search);
  return note === undefined || note === "" ? words : `${words}: ${note}`;
}

/**
 * `No related-item check was made: <reason>.` and the words to search by hand: `searchQuery`
 * when the server chose them, else the first three words of four or more letters in the title.
 * It names words rather than a search, because the palette cannot be opened without losing the
 * draft; with no such word it says what to search for instead of guessing.
 */
function notMadeText(reason: string, title: string, searchQuery?: string): string {
  const found = searchQuery?.trim() || longWords(title);
  const words = found === "" ? "two or three of its words" : `“${found}”`;
  return `No related-item check was made: ${reason}. Search Dispatch for ${words} before you rely on this being new.`;
}

/** The first three words of four or more letters: short words are mostly ones every item holds.
 *  Named apart from `features/agents/agent-search.ts`'s exported `searchWords`, which splits a
 *  query into each of its lowercased words. */
function longWords(text: string): string {
  return (text.match(/[\p{L}\p{N}_]+/gu) ?? [])
    .filter((word) => (word.match(/\p{L}/gu) ?? []).length >= 4)
    .slice(0, 3)
    .join(" ");
}
