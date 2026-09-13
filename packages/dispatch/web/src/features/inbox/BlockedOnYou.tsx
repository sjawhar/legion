import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { InboxRow } from "../../api/types";
import { linkHoverText, linkText, textMutedOnCanvas } from "../../theme/classes";
import { formatAskAge } from "./ask-age";

/** Open asks whose latest reply is not a human's are waiting for the viewer. */
export function waitingOnYou<T extends Pick<InboxRow, "last_reply">>(asks: readonly T[]): T[] {
  return asks.filter((ask) => ask.last_reply?.author.kind !== "user");
}

export function BlockedOnYou({ asks }: { asks: readonly InboxRow[] }): ReactNode {
  const waiting = waitingOnYou(asks);
  if (waiting.length === 0) return null;
  const oldest = waiting.reduce((earlier, ask) => {
    const earlierAt = Date.parse(earlier.created_at);
    const askAt = Date.parse(ask.created_at);
    return (Number.isNaN(askAt) ? Number.POSITIVE_INFINITY : askAt) <
      (Number.isNaN(earlierAt) ? Number.POSITIVE_INFINITY : earlierAt)
      ? ask
      : earlier;
  });

  const count = waiting.length;
  return (
    <p className={`mb-4 text-sm font-medium ${textMutedOnCanvas}`}>
      <Link className={`${linkText} ${linkHoverText}`} to="/">
        Blocked on you: {count} {count === 1 ? "item" : "items"}, oldest{" "}
        {formatAskAge(oldest.created_at)}
      </Link>
    </p>
  );
}
