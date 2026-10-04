import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { linkHoverText, secondaryButtonBorder, textMutedOnCanvas } from "../../theme/classes";
import { MarkdownPreview } from "../refs/MarkdownPreview";

// The phone stylesheet makes every link an inline-flex 44 px tap target, which does not truncate
// its own text; the quote therefore truncates in an inner element, whatever the link is.
const quoteClasses = `flex min-w-0 max-w-full items-center border-l-2 pl-2 text-xs ${secondaryButtonBorder} ${textMutedOnCanvas}`;

/** The one rendering of "this message answers that one": the quoted parent under a reply turn
 *  and the Replying-to chip in the composer, worded `Replying to <author> — <body>` so both
 *  read the same. The prefix is the app's own words and stays plain; the parent's body is
 *  Markdown and renders as such, bold and code and references formatted, cut to the quote's
 *  one line after rendering. Without a loaded parent the author is unknown and the quote says
 *  so. With `to` it links to the parent's turn. */
export function ReplyQuote({
  author,
  className = "",
  excerpt,
  to,
}: {
  author: string | undefined;
  className?: string;
  /** The quoted parent's body as its author wrote it (Markdown); `""` when none is known. */
  excerpt: string;
  to?: string;
}): ReactNode {
  const text = (
    <MarkdownPreview
      className="min-w-0"
      lead={`Replying to ${author ?? "a message"}${excerpt === "" ? "" : " — "}`}
      lines={1}
      markdown={excerpt}
    />
  );
  return to === undefined ? (
    <span className={`${quoteClasses} ${className}`}>{text}</span>
  ) : (
    <Link className={`${quoteClasses} ${linkHoverText} ${className}`} to={to}>
      {text}
    </Link>
  );
}
