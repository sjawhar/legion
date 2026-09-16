import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { linkHoverText, secondaryButtonBorder, textMutedOnCanvas } from "../../theme/classes";

// The phone stylesheet makes every link an inline-flex 44 px tap target, which no longer
// truncates its own text; the quote therefore truncates in an inner span, whatever the link is.
const quoteClasses = `flex min-w-0 max-w-full items-center border-l-2 pl-2 text-xs ${secondaryButtonBorder} ${textMutedOnCanvas}`;

/** The one wording of a reply quote - "Replying to <author> — <first line>" - so a turn's
 *  quoted parent and the composer's chip read the same. Without a loaded parent the author is
 *  unknown and the quote says so. */
export function replyQuoteText(author: string | undefined, excerpt: string): string {
  const prefix = `Replying to ${author ?? "a message"}`;
  return excerpt === "" ? prefix : `${prefix} — ${excerpt}`;
}

/** The first line of a message body, what a quote shows of it. */
export function firstLine(body: string): string {
  return body.trim().split("\n")[0] ?? "";
}

/** The one rendering of "this message answers that one": the quoted parent under a reply turn
 *  and the Replying-to chip in the composer. With `to` it links to the parent's turn. */
export function ReplyQuote({
  children,
  className = "",
  to,
}: {
  children: ReactNode;
  className?: string;
  to?: string;
}): ReactNode {
  const text = <span className="min-w-0 truncate">{children}</span>;
  return to === undefined ? (
    <span className={`${quoteClasses} ${className}`}>{text}</span>
  ) : (
    <Link className={`${quoteClasses} ${linkHoverText} ${className}`} to={to}>
      {text}
    </Link>
  );
}
