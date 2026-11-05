import { useQueries, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import type { ReferenceLinking } from "./RefLink";
import {
  issueSummaryQuery,
  questionReferences,
  questionsQuery,
  referencesQuery,
  resolvedPrefixItems,
} from "./reference-target";
import { readTextReferences, type TextReading, type TextSource } from "./text-references";

/** The empty answers, shared so a body that needs nothing keeps one identity across renders and
 *  the link pass it is a dependency of never re-runs for a fresh empty object. */
const NO_KEYS: readonly string[] = [];
const NO_MAP: Readonly<Record<string, string>> = {};

/** Everything a rendered body needs to link what it cites, and the reading it was found in:
 *  the one reader's pass over the body once every fact it waits on is in (`TextReading`), so a
 *  caller that needs the references themselves - the cards under a body, the editor's
 *  decorations - reads them rather than parsing the same text again. */
export interface TextReferences extends ReferenceLinking {
  readonly reading: TextReading;
}

/**
 * Everything a rendered body needs to link what it cites: the one reader says what the body's
 * source cites and what it would have to ask (`text-references.ts`), this starts exactly those
 * queries and no others, and the link pass over the painted text is held to the answer
 * (`linkifyDispatchRefs`).
 *
 * A bare issue key links only while its issue exists, which is its summary read answering: the
 * same read the link's own title comes from, so a key costs one request and a 404 for it is
 * final (`api/event-refresh.ts` re-asks it on `issue.created` alone). A `#id` resolves against
 * the question map of the document the text's own source belongs to, which the server says and
 * `questionsQuery` reads; a text naming no source, or one whose text belongs to no document,
 * links no `#id`. An id written as a prefix is resolved by the graph read of the reference as
 * written (`referencesQuery`), and the item is then read under the whole id the answer carries,
 * so nothing is ever cached under a prefix.
 *
 * A body that writes no key, no `#id` and no prefix - almost every body - starts no query at all
 * and is read once.
 */
export function useTextReferences(markdown: string, source?: TextSource): TextReferences {
  const origin = window.location.origin;
  // The first reading: it resolves every `dispatch://` reference and dashboard address on its
  // own, and says which keys, question and prefixes the rest waits on.
  const written = useMemo(() => readTextReferences(markdown, { origin }), [markdown, origin]);
  const { keys, prefixes, questions } = written.needs;

  const summaries = useQueries({
    queries: keys.map((key) => ({ ...issueSummaryQuery(key), retry: false })),
  });
  // Joined rather than kept as an array, so the facts below hold one identity while the set of
  // issues that exist is unchanged, however often the queries report.
  const exist = keys.filter((_, index) => summaries[index]?.isSuccess === true).join(" ");
  const issues = useMemo(() => (exist === "" ? NO_KEYS : exist.split(" ")), [exist]);

  // Only a text that writes a `#id` asks for the map (`needs.questions`), and only one that
  // names the record its text is can: the route answers for a source, not for a page.
  const questionRead = useQuery({
    ...questionsQuery(source),
    enabled: questions && source !== undefined,
  });
  const askedQuestions = useMemo(
    () => questionReferences(questionRead.data) ?? NO_MAP,
    [questionRead.data]
  );

  const resolutions = useQueries({
    queries: prefixes.map((reference) => ({ ...referencesQuery(reference), retry: false })),
  });
  const resolved = JSON.stringify(
    Object.assign({}, ...resolutions.map((answer) => resolvedPrefixItems(answer.data)))
  );
  const items = useMemo(
    () => (resolved === "{}" ? NO_MAP : (JSON.parse(resolved) as Record<string, string>)),
    [resolved]
  );

  const facts = useMemo(
    () => ({ issues, items, origin, questions: askedQuestions }),
    [askedQuestions, issues, items, origin]
  );
  const reading: TextReading = useMemo(
    () =>
      keys.length === 0 && prefixes.length === 0 && !questions
        ? written
        : readTextReferences(markdown, facts),
    [facts, keys.length, markdown, prefixes.length, questions, written]
  );
  return useMemo(() => {
    const cited = new Set<string>();
    for (const reference of reading.references) {
      if (reference.reference !== undefined) {
        cited.add(reference.reference);
      }
    }
    return { cited, facts, reading };
  }, [facts, reading]);
}
