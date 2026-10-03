import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { inboxQuery } from "../../api/queries";
import type { CredentialPendingRow, InboxRow } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import {
  borderDefault,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
  textSecondaryOnSurfaceMuted,
} from "../../theme/classes";
import { useCredentialRequests } from "../credentials/pending";
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

/** Everything the Inbox lists that waits for the viewer - its asks whose turn is theirs, and every
 *  pending credential request, each of which names the viewer as its approver - counted, with when
 *  the oldest of them began waiting (undefined when nothing does). The badges and the banner read
 *  this one rule, so another kind of waiting item is added here once. */
export function needsYou(
  asks: readonly InboxRow[],
  credentialRequests: readonly CredentialPendingRow[]
): { count: number; oldest: string | undefined } {
  const since = [
    ...waitingOnYou(asks).map((ask) => ask.created_at),
    ...credentialRequests.map((request) => request.requested_at),
  ];
  const oldest = since.reduce<string | undefined>(
    (earlier, at) => (earlier === undefined || compareTimestamps(at, earlier) < 0 ? at : earlier),
    undefined
  );
  return { count: since.length, oldest };
}

/** The rail's and the compact top bar's `Needs you N`, over the whole inbox; the banner counts
 *  the view. */
export function useNeedsYouCount(): number {
  const inbox = useQuery(inboxQuery());
  const { requests } = useCredentialRequests();
  return needsYou(inbox.data ?? [], requests).count;
}

export function BlockedOnYou({
  asks,
  className,
  credentialRequests = [],
  variant = "banner",
}: {
  asks: readonly InboxRow[];
  className?: string;
  /** The credential requests the Inbox lists above its asks; the project pill has none. */
  credentialRequests?: readonly CredentialPendingRow[];
  variant?: "banner" | "pill";
}): ReactNode {
  const { count, oldest } = needsYou(asks, credentialRequests);
  if (oldest === undefined) return null;

  if (variant === "pill") {
    return (
      <Link
        className={`${className ?? ""} inline-flex min-h-11 items-center rounded-full px-3 text-xs font-medium md:min-h-9 md:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary}`}
        to="/"
      >
        Blocked on you · {count}
      </Link>
    );
  }

  // A count of every waiting ask, not one urgency, so the strip stays structural: the spec
  // reserves the urgency hues for urgency and priority. `w-full` is load-bearing - below
  // 1280 px an unlayered `a:not(.prose a) { display: inline-flex }` in styles.css beats
  // Tailwind's `block`, and without it the strip sits beside the Mine/Everyone switch.
  return (
    <Link
      className={`block w-full rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnSurfaceMuted}`}
      to="/"
    >
      Blocked on you: {count} {count === 1 ? "item" : "items"}, oldest {formatAskAge(oldest)}
    </Link>
  );
}
