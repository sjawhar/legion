import { relatedCheckFormOf, relatedCheckSearchWords } from "@legion/contracts";
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
import { RELATED_DEBOUNCE_MS, RELATED_TITLE_MIN } from "./related-timing";

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
    // This check opts out of the app's `staleTime: 30_000` (`web/src/main.tsx`) and of
    // react-query's five-minute `gcTime`: nothing invalidates this key, and the answer is
    // advice about what exists right now, so an answer kept from before an issue was filed
    // would tell the reader the opposite of the truth. Every settled title is asked fresh, and
    // the debounce bounds how often that is.
    gcTime: 0,
    queryFn: ({ signal }) => api.relatedItems(project, debounced, { signal }),
    queryKey: ["related", project, debounced],
    staleTime: 0,
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
 * announced rather than silently replacing the one a screen reader just read; while a check is
 * out it is `aria-busy`, so a reader hears the answer alone and not "Checking for related
 * items…" at every pause in their typing.
 */
export function RelatedItems({ project, title }: { project: string; title: string }): ReactNode {
  const { query, shown, trimmed, waiting } = useRelatedItems(project, title);
  const failed = shown && !waiting && query.isError;
  // The answer to the title in the box, once there is one: while a check is out, or while the
  // box has moved past the title the query answers, there is none to show.
  const answer = failed || waiting ? undefined : query.data;
  const checking = shown && !failed && answer === undefined;
  let body: ReactNode;
  if (!shown) {
    body = null;
  } else if (failed) {
    body = (
      <QueryError
        message="Could not check for related items."
        onRetry={() => void query.refetch()}
        retrying={query.isFetching}
      />
    );
  } else if (answer === undefined) {
    body = <p className={textMutedOnSurface}>Checking for related items…</p>;
  } else {
    body = <RelatedForm suggestions={answer} title={trimmed} />;
  }
  return (
    <section
      aria-busy={checking}
      aria-label="Related items"
      aria-live="polite"
      className="space-y-2 text-sm"
    >
      {body}
    </section>
  );
}

/**
 * Exactly one of the check's three forms, then the past decision when the check found one.
 * Which form the answer holds is the contract's judgement (`relatedCheckFormOf`), the same one
 * the CLI reads; this only renders it. `title_matches` is not shown: this route writes nothing,
 * so no title overlap was forced past.
 */
function RelatedForm({
  suggestions,
  title,
}: {
  suggestions: Suggestions;
  title: string;
}): ReactNode {
  const { decision } = suggestions;
  const check = relatedCheckFormOf(suggestions);
  let form: ReactNode;
  if (check.form === "not_made") {
    form = (
      <p className={textMutedOnSurface}>{notMadeText(check.reason, title, check.searchQuery)}</p>
    );
  } else if (check.form === "items") {
    form = (
      <>
        <p className={`font-medium ${textSecondaryOnSurface}`}>
          {/* A server predating the forms names no search mode, so the heading names none. */}
          {check.mode === undefined
            ? "Related items:"
            : `Related items (searched by ${check.mode}):`}
        </p>
        <ul className="space-y-2">
          {check.related.map((item) => (
            <RelatedItemRow item={item} key={`${item.kind}:${item.id}`}>
              {item.kind === "ask" && item.ask !== undefined ? (
                <AskHitStatus ask={item.ask} muted={false} />
              ) : null}
            </RelatedItemRow>
          ))}
        </ul>
      </>
    );
  } else {
    form = (
      <p className={textMutedOnSurface}>
        {check.mode === undefined
          ? "Related-item check searched and found nothing."
          : `Related-item check searched (${check.mode}) and found nothing.`}
      </p>
    );
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
            <RelatedItemRow item={decision}>
              <p className={`text-xs ${textMutedOnSurface}`}>
                Answered by {decision.answered_by}
                {decision.answered_at === undefined ? null : (
                  <>
                    {" · "}
                    <Timestamp at={decision.answered_at} />
                  </>
                )}
              </p>
              {/* One item per owner can collapse the decision's ask out of the related items,
                  so where its ask stands is named here, inside the decision's own item. */}
              {decision.ask === undefined ? null : (
                <AskHitStatus ask={decision.ask} muted={false} />
              )}
            </RelatedItemRow>
          </ul>
        </div>
      )}
    </>
  );
}

/**
 * One item: its kind, where it stands, and a link whose whole text is the owner's label, so a
 * screen reader's link list names the item and nothing else. It opens in a new tab, since the
 * draft in the dialog would be lost by navigating away from it. `children` renders under the
 * link, inside this item, so whatever an owner adds about the item is read as part of it.
 */
function RelatedItemRow({
  children,
  item,
}: {
  children: ReactNode;
  item: WriteSuggestion;
}): ReactNode {
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
      {children}
    </li>
  );
}

/**
 * `No related-item check was made: <reason>.` and the words to search by hand: `searchQuery`
 * when the server chose them, else the first three words of four or more letters in the title.
 * It names words rather than a search, because the palette cannot be opened without losing the
 * draft; with no such word it says what to search for instead of guessing.
 */
function notMadeText(reason: string, title: string, searchQuery?: string): string {
  const found = searchQuery?.trim() || relatedCheckSearchWords(title);
  const words = found === "" ? "two or three of its words" : `“${found}”`;
  return `No related-item check was made: ${reason}. Search Dispatch for ${words} before you rely on this being new.`;
}
