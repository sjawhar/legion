import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { userAgentStateQuery } from "../../api/queries";
import type { Agent } from "../../api/types";
import { LabelPill } from "../../components/Pill";
import {
  borderDefault,
  card,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { sessionLabel } from "../refs/actor";

import { unreadRepliesLabel } from "./unread";

/**
 * Sessions that answered the viewer and are no longer in the live list. The unread count sums
 * every session that replied, and a session often answers and then exits, so each of them keeps
 * a row here whose Open reads its replies in the live view: the badge is always one the viewer
 * can clear, and a reply is never lost because its session ended.
 */
export function EndedAgentsWithReplies({ live }: { live: readonly Agent[] }): ReactNode {
  const states = useQuery(userAgentStateQuery()).data ?? {};
  const ended = Object.entries(states).filter(
    ([sessionID, state]) =>
      state.unread_replies > 0 && !live.some((agent) => agent.session_id === sessionID)
  );
  if (ended.length === 0) return null;
  const title = "Replied, no longer connected";
  return (
    <section aria-label={title} className="mt-5 space-y-3">
      <h2 className={`text-xs font-semibold uppercase ${textMutedOnCanvas}`}>{title}</h2>
      {ended.map(([sessionID, state]) => {
        const label = sessionLabel(sessionID, "");
        const to = `/agents/${encodeURIComponent(sessionID)}/live`;
        return (
          <article
            className={`flex flex-wrap items-center gap-x-3 gap-y-1 rounded-xl border px-3 py-1.5 ${card} ${borderDefault}`}
            key={sessionID}
          >
            <span className={`min-w-0 truncate text-sm font-semibold ${textPrimaryOnCanvas}`}>
              {label}
            </span>
            <Link
              aria-label={`Open ${label}`}
              className={`min-h-11 rounded-lg border px-2 text-xs font-medium whitespace-nowrap md:min-h-7 md:leading-7 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              title={`Read ${label}'s replies`}
              to={to}
            >
              Open
            </Link>
            <span className="ml-auto">
              <LabelPill selected>{unreadRepliesLabel(state.unread_replies)}</LabelPill>
            </span>
          </article>
        );
      })}
    </section>
  );
}
