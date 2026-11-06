import type { ReactNode } from "react";

import { useCopyFeedback } from "../../hooks/useCopyFeedback";
import {
  dangerText,
  successText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { DispatchPicture } from "../refs/DispatchPicture";
import { parseNodeReference } from "../refs/routes";
import type { NodeFact } from "./issue-tree";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS, nodeFactLabel } from "./page-words";

/**
 * The facts a node's body draws under its text (dispatch://INFRA-1196 R15,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t16`): a check's four attributes and what a
 * recorded result says of itself (`check-facts.ts`).
 *
 * It is a two-column grid and not a wrapping row, which is the cause of most of the sixteen
 * faults measured on the old Verification tab
 * (`dispatch://INFRA-1196/artifact/design-notes-md` section 5): a label never wraps away from
 * its value, and every value sits in a column of its own that may be zero wide
 * (`min-w-0`) and breaks inside a word where it has nothing else to break on, so nothing
 * reaches past the card at 390 px and no two pieces of text are drawn over each other.
 *
 * A `run` is a command: a monospace block with its own copy control, marked `[data-node-command]`
 * (`e2e/issue-page.ts`). A screenshot is a picture, drawn through the same bytes route every
 * other picture of Dispatch is drawn through (`DispatchPicture`); one whose reference names
 * nothing the browser can draw reads as a link to it, as it does in a document.
 */
export function NodeFacts({ facts }: { facts: readonly NodeFact[] }): ReactNode {
  if (facts.length === 0) return null;
  return (
    <dl className="grid min-w-0 grid-cols-[auto_minmax(0,1fr)] gap-x-2 gap-y-0.5 text-xs">
      {facts.map((fact, index) => (
        <Fact
          fact={fact}
          // A node may record two screenshots and nothing else tells them apart, so the key is
          // the fact's own place in the list.
          // biome-ignore lint/suspicious/noArrayIndexKey: a fact has no id of its own
          key={`${fact.name}:${index}`}
        />
      ))}
    </dl>
  );
}

function Fact({ fact }: { fact: NodeFact }): ReactNode {
  return (
    <>
      <dt className={`min-w-0 break-words ${textMutedOnCanvas}`} {...pageChrome}>
        {nodeFactLabel(fact.name)}
      </dt>
      <dd className="m-0 min-w-0">
        <FactValue fact={fact} />
      </dd>
    </>
  );
}

/** A value drawn as what it is: the page's own word, a command, a picture, or the author's own
 *  text, which breaks inside a word where a long value has nothing else to break on. */
function FactValue({ fact }: { fact: NodeFact }): ReactNode {
  if (fact.value === "") {
    // A check that names no negative control says so by keeping the name with no value, which
    // is itself what a reader came to see.
    return (
      <span className={textMutedOnCanvas} {...pageChrome}>
        —
      </span>
    );
  }
  if (fact.draw === "command") {
    return <Command command={fact.value} />;
  }
  if (fact.draw === "picture") {
    return <Picture reference={fact.value} />;
  }
  return (
    <span
      className={`block min-w-0 break-words ${textSecondaryOnCanvas}`}
      {...(fact.draw === "word" ? pageChrome : authorText)}
    >
      {fact.value}
    </span>
  );
}

/** A command, in a monospace block a reader can read and copy whole: it wraps inside the body
 *  rather than scrolling, since a row has to fit the screen after every move of the keyboard
 *  (R1, R17). The copy control takes no tab stop - the tree's one tab stop is its rows - and
 *  carries no `title`, because a reader on a phone or on the keyboard never sees one (R2). */
function Command({ command }: { command: string }): ReactNode {
  const { copy, status } = useCopyFeedback();
  return (
    <span className="flex min-w-0 items-start gap-1">
      <code
        className={`min-w-0 flex-1 rounded px-1 font-mono break-all whitespace-pre-wrap ${surfaceMutedBg} ${textSecondaryOnCanvas}`}
        data-node-command=""
        {...authorText}
      >
        {command}
      </code>
      <button
        aria-label={CONTROL_LABELS.copy_command}
        className={`shrink-0 underline ${textMutedOnCanvas}`}
        onClick={() => copy(command)}
        tabIndex={-1}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.copy}
      </button>
      {status === "idle" ? null : (
        <span
          aria-live="polite"
          className={`shrink-0 ${status === "copied" ? successText : dangerText}`}
          {...pageChrome}
        >
          {status === "copied" ? CONTROL_LABELS.copied : CONTROL_LABELS.copy_failed}
        </span>
      )}
    </span>
  );
}

/** A screenshot a result recorded, as the picture it is. */
function Picture({ reference }: { reference: string }): ReactNode {
  const route = parseNodeReference(reference);
  if (route === undefined) {
    return (
      <span className={`block min-w-0 break-all ${textSecondaryOnCanvas}`} {...authorText}>
        {reference}
      </span>
    );
  }
  return (
    <span className="block min-w-0" {...authorText}>
      <DispatchPicture caption="" route={route} variant="block" />
    </span>
  );
}
