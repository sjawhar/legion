import type { ReactNode } from "react";

import { borderDefault, textMutedOnSurface, textPrimaryOnSurface } from "../../theme/classes";
import type { FocusedNode } from "./focused-node";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS, NODE_TYPE_NAMES, sentenceCase } from "./page-words";

/**
 * The facts of whatever the reader's focus is on, in the panel (dispatch://INFRA-1196 R12): what
 * it is, how it stands, who made it and what it says. They change as the focus moves, because
 * they are read from the focused node itself (`focused-node.tsx`); until the tree lands the
 * focus is the issue, so this draws the issue's own facts rather than nothing.
 */
export function FocusedNodeFacts({ node }: { node: FocusedNode }): ReactNode {
  return (
    <section
      className={`border-b pb-2 ${borderDefault}`}
      data-focused-node={node.id}
      data-testid="focused-node-facts"
    >
      <h2 className={`text-sm font-semibold ${textPrimaryOnSurface}`} {...pageChrome}>
        {PROPERTY_LABELS.focused_node}
      </h2>
      <dl className="mt-1 flex min-w-0 flex-col gap-1 text-sm">
        <Fact label={sentenceCase(NODE_TYPE_NAMES[node.type])} value={node.title} />
        {node.state === undefined ? null : (
          <Fact chrome label={PROPERTY_LABELS.status} value={node.state} />
        )}
        {node.author === undefined ? null : (
          <Fact label={CONTROL_LABELS.written_by} value={node.author} />
        )}
      </dl>
    </section>
  );
}

/** One fact: its name in the page's words, its value in whoever's words it came from. A state
 *  word is the page's own (`NODE_STATE_WORDS`); a title and an author's name are not. */
function Fact({
  chrome = false,
  label,
  value,
}: {
  chrome?: boolean;
  label: string;
  value: string;
}): ReactNode {
  return (
    <div className="flex min-w-0 flex-wrap items-baseline gap-2">
      <dt className={`font-medium ${textMutedOnSurface}`} {...pageChrome}>
        {label}:
      </dt>
      <dd className="min-w-0 break-words" {...(chrome ? pageChrome : authorText)}>
        {value}
      </dd>
    </div>
  );
}
