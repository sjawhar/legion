import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { InboxRow } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import {
  borderDefault,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
  textSecondaryOnSurfaceMuted,
} from "../../theme/classes";
import { formatAskAge } from "./ask-age";
import { isSnoozed } from "./snooze";

/** Open asks whose turn is the human's (`waiting_on === "human"`) and that the viewer has not
 *  deferred are waiting for the viewer. An agent's progress note keeps its ask waiting on the
 *  agent, whoever replied last; a snooze keeps its ask off every "needs you" surface - the nav
 *  badge, the sidebar, this banner - until it returns, because deferring it is the viewer
 *  saying it does not need them now. A row that carries no snooze at all (an issue header's
 *  `open_asks`) is never deferred. */
export function waitingOnYou<
  T extends Pick<InboxRow, "waiting_on"> & Partial<Pick<InboxRow, "snoozed_until">>,
>(asks: readonly T[]): T[] {
  return asks.filter((ask) => ask.waiting_on === "human" && !isSnoozed(ask));
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

  const oldest = waiting.reduce((earlier, ask) =>
    compareTimestamps(ask.created_at, earlier.created_at) < 0 ? ask : earlier
  );

  const count = waiting.length;
  // A count of every waiting ask, not one urgency, so the strip stays structural: the spec
  // reserves the urgency hues for urgency and priority. `w-full` is load-bearing - below
  // 1280 px an unlayered `a:not(.prose a) { display: inline-flex }` in styles.css beats
  // Tailwind's `block`, and without it the strip sits beside the Mine/Everyone switch.
  return (
    <Link
      className={`block w-full rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnSurfaceMuted}`}
      to="/"
    >
      Blocked on you: {count} {count === 1 ? "item" : "items"}, oldest{" "}
      {formatAskAge(oldest.created_at)}
    </Link>
  );
}
