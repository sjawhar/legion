import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Ask } from "../../api/types";
import { linkHoverText, linkText, textMutedOnCanvas } from "../../theme/classes";
import { formatAskAge } from "./ask-age";

function openedAt(ask: Ask): number {
  const value = Date.parse(ask.created_at);
  return Number.isNaN(value) ? Number.POSITIVE_INFINITY : value;
}

/** Open asks whose latest reply is not a human's are waiting for the viewer. */
export function waitingOnYou(asks: readonly Ask[]): Ask[] {
  return asks
    .filter((ask) => ask.last_reply?.author.kind !== "user")
    .sort((left, right) => openedAt(left) - openedAt(right));
}

export function BlockedOnYou({ asks }: { asks: readonly Ask[] }): ReactNode {
  const waiting = waitingOnYou(asks);
  const oldest = waiting[0];
  if (oldest === undefined) return null;

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
