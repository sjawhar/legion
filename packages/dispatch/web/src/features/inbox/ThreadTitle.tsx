import type { ReactNode } from "react";

import type { MyReplyRow } from "../../api/types";
import { textMutedOnCanvas, textPrimaryOnCanvas, unreadDotBg } from "../../theme/classes";
import { questionLine } from "./question-line";

/**
 * What a thread row is about, in one line, bold with a dot while the thread holds a write the
 * reader has not read.
 *
 * Both layouts draw it: the short row beside the reading column, where it is the link `o`
 * follows (`data-inbox-owner`), and the card below that width, where **Open** is that link and
 * the title is plain text. Each host sizes its own line, as the three-site owner line does
 * (`ThreadOwnerLine` in `AskOwnerLink.tsx`); what unread looks like is this one place.
 */
export function ThreadTitle({
  className,
  href,
  thread,
}: {
  /** The classes the host's line gives the title; the unread weight is added to them. */
  className: string;
  /** Where the title links, for the host that draws it as a link; plain text without one. */
  href?: string;
  thread: MyReplyRow;
}): ReactNode {
  const title = questionLine(thread.subject);
  const text = `${className} ${
    thread.unread ? `font-semibold ${textPrimaryOnCanvas}` : textMutedOnCanvas
  }`;
  return (
    <>
      {href === undefined ? (
        <span className={text} title={title}>
          {title}
        </span>
      ) : (
        <a className={text} data-inbox-owner="" href={href} title={title}>
          {title}
        </a>
      )}
      {thread.unread ? (
        <span
          aria-label="Unread"
          className={`size-2 shrink-0 rounded-full ${unreadDotBg}`}
          role="img"
        />
      ) : null}
    </>
  );
}
