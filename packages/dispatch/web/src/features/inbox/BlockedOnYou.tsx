import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { InboxRow } from "../../api/types";
import {
  linkHoverText,
  linkText,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { formatAskAge } from "./ask-age";

/** Open asks whose turn is the human's (`waiting_on === "human"`) are waiting for the viewer. An
 *  agent's progress note keeps its ask waiting on the agent, whoever replied last. */
export function waitingOnYou<T extends Pick<InboxRow, "waiting_on">>(asks: readonly T[]): T[] {
  return asks.filter((ask) => ask.waiting_on === "human");
}

export function BlockedOnYou({
  asks,
  className,
  variant = "banner",
}: {
  asks: readonly InboxRow[];
  className?: string;
  variant?: "banner" | "pill";
}): ReactNode {
  const waiting = waitingOnYou(asks);
  if (waiting.length === 0) return null;

  if (variant === "pill") {
    return (
      <Link
        className={`${className ?? ""} inline-flex min-h-11 items-center rounded-full px-3 text-xs font-medium md:min-h-9 md:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary}`}
        to="/"
      >
        Blocked on you · {waiting.length}
      </Link>
    );
  }

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
