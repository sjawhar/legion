import type { ReplyBlock } from "@legion/contracts";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { Actor } from "../../api/types";
import { linkHoverText, secondaryButtonBorder, textMutedOnCanvas } from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import { MarkdownPreview } from "../refs/MarkdownPreview";

// The phone stylesheet makes every link an inline-flex 44 px tap target, which does not truncate
// its own text; the quote therefore truncates in an inner element, whatever the link is.
const quoteClasses = `flex min-w-0 max-w-full items-center border-l-2 pl-2 text-xs ${secondaryButtonBorder} ${textMutedOnCanvas}`;

/** The comment or message among `items` (the thread a reply sits in) that `block`, the reply's
 *  `reply_block`, is a block of: its node reads `<kind>/<id>#<block id>`. A comment reply's
 *  `reply_to` is its thread's head, so the passage it answers can be another reply's. */
export function passageItem<T extends { readonly id: string }>(
  block: ReplyBlock,
  items: readonly T[]
): T | undefined {
  const id = block.node.slice(block.node.indexOf("/") + 1, block.node.lastIndexOf("#"));
  return items.find((item) => item.id === id);
}

/** The one rendering of "this message answers that one": the quoted parent under a reply turn
 *  and the Replying-to chip in the composer, worded `Replying to <author> — <body>` so both
 *  read the same. The prefix is the app's own words and stays plain; the parent's body is
 *  Markdown and renders as such, bold and code and references formatted, cut to the quote's
 *  one line after rendering. Without a loaded parent the author is unknown and the quote says
 *  `Replying to a message`. With `to` it links to the parent's turn.
 *
 *  A reply that names one block of its parent (`block`, the reply's `reply_block`) quotes that
 *  passage instead, worded `Replying to <author>'s passage — <text>`; once the parent's edit
 *  removed the passage, the quote says so in place of its text.
 *
 *  The author is drawn by `AgentName`, whose name links to the agent, so it sits outside the
 *  quote's link, which holds the rest: the body, or with no author the whole line. */
export function ReplyQuote({
  author,
  block,
  className = "",
  excerpt,
  to,
}: {
  author: Actor | undefined;
  /** The block of the parent the reply answers; null or absent when it answers the whole. */
  block?: ReplyBlock | null;
  className?: string;
  /** The quoted parent's body as its author wrote it (Markdown); `""` when none is known. */
  excerpt: string;
  to?: string;
}): ReactNode {
  const passage = block !== undefined && block !== null;
  const quotedText = passage ? block.text : excerpt;
  const unknownParent = passage ? "Replying to a passage" : "Replying to a message";
  // The dash opens the quoted body, so the body is never an element of its own text: a reader
  // (or a test) matching the parent's body finds the parent's turn, not its every quote.
  const quoted =
    passage && block.removed ? (
      <span className="block truncate">
        {author === undefined ? `${unknownParent} — ` : "— "}
        This passage was removed.
      </span>
    ) : author === undefined || quotedText !== "" ? (
      <MarkdownPreview
        className="min-w-0"
        lead={author === undefined ? `${unknownParent}${quotedText === "" ? "" : " — "}` : "— "}
        lines={1}
        markdown={quotedText}
      />
    ) : null;
  return (
    <span className={`${quoteClasses} gap-1 ${className}`}>
      {author === undefined ? null : (
        <span className="min-w-0 truncate">
          Replying to <AgentName actor={author} inline />
          {passage ? "'s passage" : ""}
          {quoted === null ? "" : " "}
        </span>
      )}
      {quoted === null ? null : to === undefined ? (
        <span className="min-w-0 flex-1">{quoted}</span>
      ) : (
        <Link className={`min-w-0 flex-1 ${linkHoverText}`} to={to}>
          {quoted}
        </Link>
      )}
    </span>
  );
}
