import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { inboxQuery, whoAmIQuery } from "../../api/queries";
import type { CredentialPendingRow, InboxRow } from "../../api/types";
import { compareTimestamps, isTimestamp } from "../../lib/timestamps";
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
import { isMine } from "./sections";
import { isSnoozed } from "./snooze";

/** Open asks whose turn is a person's (`waiting_on === "human"`) and that the viewer has not
 *  deferred. Whose turn it is, not whose question it is: who answers it is the issue's assignee,
 *  which `needsYou` below adds. An agent's progress note keeps its ask waiting on the
 *  agent, whoever replied last; a snooze keeps its ask off every "needs you" surface - the nav
 *  badge, the sidebar, this banner - until it returns, because deferring it is the viewer
 *  saying it does not need them now. A row that carries no snooze at all (an issue header's
 *  `open_asks`) is never deferred. */
export function waitingOnHuman<
  T extends Pick<InboxRow, "waiting_on"> & Partial<Pick<InboxRow, "snoozed_until">>,
>(asks: readonly T[]): T[] {
  return asks.filter((ask) => ask.waiting_on === "human" && !isSnoozed(ask));
}

/** Everything that waits for the viewer themselves - every open ask whose turn is a person's, on
 *  an issue assigned to them (`isMine`, the same rule the list's own filter reads), that they
 *  have not deferred, and every pending credential request they may decide, as its approver or
 *  as anyone signed in - counted, with when the oldest of them began waiting (undefined when
 *  nothing does). The badges and the banner read this one rule, so every count of work waiting
 *  on the reader means the same thing whatever the list on screen is filtered to, and another
 *  kind of waiting item is added here once. A question on nobody's issue, and every document
 *  question (a document has no assignee), is nobody's, so it waits on nobody here. Nothing is
 *  counted until the login resolves: a number taken over the whole inbox while the shell loads
 *  would not be the reader's. A time `compareTimestamps` refuses still counts but is never the
 *  oldest: the broker's `requested_at` reaches here verbatim, and the badges render in the app
 *  shell, outside every page's error boundary, so a throw here would take down every page. */
function needsYou(
  asks: readonly InboxRow[],
  credentialRequests: readonly CredentialPendingRow[],
  viewer: string | undefined
): { count: number; oldest: string | undefined } {
  if (viewer === undefined) return { count: 0, oldest: undefined };
  const since = [
    ...waitingOnHuman(asks)
      .filter((ask) => isMine(ask, viewer))
      .map((ask) => ask.created_at),
    ...credentialRequests.map((request) => request.requested_at),
  ];
  const oldest = since
    .filter(isTimestamp)
    .reduce<string | undefined>(
      (earlier, at) => (earlier === undefined || compareTimestamps(at, earlier) < 0 ? at : earlier),
      undefined
    );
  return { count: since.length, oldest };
}

/** The rail's and the compact top bar's `Needs you N`: the reader's own, over the whole inbox. */
export function useNeedsYouCount(): number {
  const inbox = useQuery(inboxQuery());
  // `/auth/whoami` names the person by lowercase email, the form issue assignees carry; until it
  // answers the rail shows no number rather than one that is not theirs.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const { requests } = useCredentialRequests();
  return needsYou(inbox.data ?? [], requests, viewer).count;
}

export function BlockedOnYou({
  asks,
  className,
  credentialRequests = [],
  variant = "banner",
  viewer,
}: {
  asks: readonly InboxRow[];
  className?: string;
  /** The credential requests the Inbox lists above its asks; the project pill has none. */
  credentialRequests?: readonly CredentialPendingRow[];
  variant?: "banner" | "pill";
  /** The signed-in person's lowercase login, whose questions this counts; nothing is counted
   *  until the caller's `/auth/whoami` read resolves. */
  viewer: string | undefined;
}): ReactNode {
  const { count, oldest } = needsYou(asks, credentialRequests, viewer);
  if (count === 0) return null;

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
      Blocked on you: {count} {count === 1 ? "item" : "items"}
      {oldest === undefined ? null : `, oldest ${formatAskAge(oldest)}`}
    </Link>
  );
}
