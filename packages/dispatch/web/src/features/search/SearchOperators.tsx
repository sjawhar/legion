import {
  SEARCH_OPERATOR_EXCLUDE,
  SEARCH_OPERATOR_OR,
  SEARCH_OPERATOR_PHRASE,
} from "@legion/contracts";
import type { ReactNode } from "react";

import type { SearchOperator } from "../../api/types";
import { badgeLow, surfaceMutedBg, textMutedOnSurface } from "../../theme/classes";

/** The three operator forms the search reads, each as a person writes it and what it does to
 *  the list. They are offered while the query is typed, so nobody has to know them already. */
const operatorForms: readonly { form: string; name: SearchOperator; what: string }[] = [
  { form: '"two words"', name: SEARCH_OPERATOR_PHRASE, what: "the phrase, in that order" },
  { form: "this OR that", name: SEARCH_OPERATOR_OR, what: "either word" },
  { form: "-word", name: SEARCH_OPERATOR_EXCLUDE, what: "hits without that word" },
];

/**
 * What the words in the box can do, under the box while a person types them. Which forms this
 * query uses is the answer's own reading of it (`query.operators`), never a second parse in the
 * browser: the search and the bar then agree about what was written, even where they would read
 * the same text differently.
 */
export function SearchOperators({ inUse }: { inUse: readonly SearchOperator[] }): ReactNode {
  return (
    <ul
      aria-label="What the words can do"
      className={`flex shrink-0 flex-wrap items-center gap-x-4 gap-y-1 px-3 py-2 text-xs ${textMutedOnSurface}`}
      data-testid="search-operators"
    >
      {operatorForms.map(({ form, name, what }) => {
        const used = inUse.includes(name);
        return (
          <li
            className="inline-flex items-center gap-1.5"
            data-in-use={used}
            data-operator={name}
            key={name}
          >
            <code className={`rounded px-1 py-0.5 ${surfaceMutedBg}`}>{form}</code>
            <span>{what}</span>
            {used ? (
              <span
                className={`rounded-full px-2 py-0.5 font-medium ${badgeLow.bg} ${badgeLow.text}`}
                data-testid={`search-operator-in-use-${name}`}
              >
                in use
              </span>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}
