import type { ReactNode } from "react";

/**
 * A link's text that ends in an ellipsis when it does not fit. Below 1280 px `styles.css` makes
 * every link an inline-flex box, and `text-overflow` never applies to a flex container's own
 * text, so a `truncate` link alone cuts its name mid-word with no ellipsis. This span is the item
 * the ellipsis applies to there; from 1280 px it is inline and the link's own `truncate` does it.
 */
export function TruncatedText({ children }: { children: ReactNode }): ReactNode {
  return <span className="truncate">{children}</span>;
}
