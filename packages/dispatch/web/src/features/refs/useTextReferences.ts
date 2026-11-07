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
import {
  readTextCandidates,
  readTextReferences,
  type TextReading,
  type TextReferenceFacts,
  type TextSource,
} from "./text-references";

/** The one reading of what a caller handed over: a body's markdown, read as one text, or the
 *  distinct candidates a document editor reported, each read as the whole mention it is. */
function readText(text: string | readonly string[], facts: TextReferenceFacts): TextReading {
  return typeof text === "string"
    ? readTextReferences(text, facts)
    : readTextCandidates(text, facts);
}

/** The empty question map, shared so a body that needs none keeps one identity across renders
 *  and the link pass it is a dependency of never re-runs for a fresh empty object. */
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
 * `text` is a body's markdown, or the distinct candidates a document editor's own reader found,
 * which are read one by one rather than joined into a text (`readTextCandidates`).
 *
 * A body that writes no key, no `#id` and no prefix - almost every body - starts no query at all
 * and is read once.
 */
export function useTextReferences(
  text: string | readonly string[],
  source?: TextSource
): TextReferences {
  const origin = window.location.origin;
  // The first reading: it resolves every `dispatch://` reference and dashboard address on its
  // own, and says which keys, question and prefixes the rest waits on.
  const written = useMemo(() => readText(text, { origin }), [origin, text]);
  const { keys, prefixes, questions } = written.needs;

  // `combine` is held to its last answer by structural sharing, so these keep one identity
  // while the set of issues that exist is unchanged, however often the queries report.
  const issues = useQueries({
    queries: keys.map((key) => ({ ...issueSummaryQuery(key), retry: false })),
    combine: (summaries): readonly string[] =>
      keys.filter((_, index) => summaries[index]?.isSuccess === true),
  });

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

  const items = useQueries({
    queries: prefixes.map((reference) => ({ ...referencesQuery(reference), retry: false })),
    combine: (resolutions): Readonly<Record<string, string>> => {
      const resolved: Record<string, string> = {};
      for (const answer of resolutions) {
        Object.assign(resolved, resolvedPrefixItems(answer.data));
      }
      return resolved;
    },
  });

  const facts = useMemo(
    () => ({ issues, items, origin, questions: askedQuestions }),
    [askedQuestions, issues, items, origin]
  );
  const reading: TextReading = useMemo(
    () =>
      keys.length === 0 && prefixes.length === 0 && !questions ? written : readText(text, facts),
    [facts, keys.length, prefixes.length, questions, text, written]
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
