import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";

import { userAgentStateQuery } from "../../api/queries";
import { railHoverBg, railNeedsYouBadgeBg, railNeedsYouBadgeText } from "../../theme/classes";
import { totalUnreadReplies, unreadRepliesLabel } from "../agents/unread";
import { AfterTheFirstBatch } from "../doc/AfterTheFirstBatch";
import { useNeedsYouCount } from "../inbox/BlockedOnYou";
import { REPLIES_PATH, routeIsInbox } from "../refs/routes";
import { useRepliesCount } from "../replies/useRepliesCount";

/** The shape every count in the compact header takes, so the box one of them stands in is the
 *  box the next one takes. */
const countBadge = "whitespace-nowrap rounded-full px-2 py-1 text-xs font-semibold";

/**
 * The inbox the compact header offers, and what it is holding for the reader.
 *
 * On the Inbox page the badge stays plain text: the page is the Inbox already, so there is
 * nothing for it to open, and at a count of nothing there is nothing to say.
 */
function InboxPeek({
  count,
  onInboxPage,
  onOpen,
}: {
  count: number;
  onInboxPage: boolean;
  onOpen: () => void;
}): ReactNode {
  if (onInboxPage) {
    return count === 0 ? null : (
      <span className={`${countBadge} ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}>
        Needs you {count}
      </span>
    );
  }
  return (
    <button
      aria-label={count === 0 ? "Open inbox" : `Open inbox, needs you ${count}`}
      className={`${countBadge} ${
        count === 0 ? railHoverBg : `${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`
      }`}
      onClick={onOpen}
      type="button"
    >
      {count === 0 ? "Inbox" : `Needs you ${count}`}
    </button>
  );
}

/**
 * What the compact header says beside the title: the inbox peek, how many asks need the reader,
 * and how many agent replies and comment replies they have not read.
 *
 * Three reads stand behind those numbers and none of them is what a reader who opened a
 * document is waiting for, so on a document address they are made - and these counts drawn -
 * after the editor's first batch (dispatch://INFRA-341 R11,
 * `features/doc/AfterTheFirstBatch.tsx`). The space they keep is the peek at a count of
 * nothing, which is exactly what the header draws today until those reads answer, so the box
 * is the box it already was and the header's height does not change as they land.
 *
 * Its own component, so a count arriving re-renders the counts rather than the whole shell.
 */
export function ShellCounts({
  onOpenInboxDrawer,
  reached,
}: {
  onOpenInboxDrawer: () => void;
  reached: boolean;
}): ReactNode {
  const { pathname } = useLocation();
  const onInboxPage = routeIsInbox(pathname);
  return (
    <AfterTheFirstBatch
      reached={reached}
      space={<InboxPeek count={0} onInboxPage={onInboxPage} onOpen={onOpenInboxDrawer} />}
    >
      <TheCounts onInboxPage={onInboxPage} onOpen={onOpenInboxDrawer} />
    </AfterTheFirstBatch>
  );
}

function TheCounts({
  onInboxPage,
  onOpen,
}: {
  onInboxPage: boolean;
  onOpen: () => void;
}): ReactNode {
  const needsYouCount = useNeedsYouCount();
  const unreadReplies = totalUnreadReplies(useQuery(userAgentStateQuery()).data);
  const unreadThreads = useRepliesCount();
  return (
    <>
      <InboxPeek count={needsYouCount} onInboxPage={onInboxPage} onOpen={onOpen} />
      {unreadReplies === 0 ? null : (
        <Link
          className={`${countBadge} ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}
          to="/agents"
        >
          {unreadRepliesLabel(unreadReplies)}
        </Link>
      )}
      {unreadThreads === 0 ? null : (
        <Link
          className={`${countBadge} ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}
          to={REPLIES_PATH}
        >
          Replies {unreadThreads}
        </Link>
      )}
    </>
  );
}
